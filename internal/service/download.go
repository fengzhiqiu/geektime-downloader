package service

import (
	"context"
	"fmt"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/config"
	coursedl "github.com/nicoxiang/geektime-downloader/internal/course"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/pdf"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
)

// LookupRequest identifies a course to look up.
type LookupRequest struct {
	ProductType string `json:"product_type"`
	ProductID   int    `json:"product_id"`
	Enterprise  bool   `json:"enterprise"`
}

// DownloadOptions overrides default download settings for a single job.
type DownloadOptions struct {
	DownloadFolder     string `json:"download_folder,omitempty"`
	Quality            string `json:"quality,omitempty"`
	Output             int    `json:"output,omitempty"`
	Comments           int    `json:"comments,omitempty"`
	Interval           int    `json:"interval,omitempty"`
	Overwrite          bool   `json:"overwrite"`
	PrintPDFWait       int    `json:"print_pdf_wait,omitempty"`
	PrintPDFTimeout    int    `json:"print_pdf_timeout,omitempty"`
}

// DownloadRequest creates an async download job.
type DownloadRequest struct {
	ProductType    string          `json:"product_type"`
	ProductID      int             `json:"product_id"`
	Enterprise     bool            `json:"enterprise"`
	Mode           string          `json:"mode"`
	ArticleIDs     []int           `json:"article_ids,omitempty"`
	Options        DownloadOptions `json:"options"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
}

// DownloadService orchestrates course lookup and downloads without UI dependencies.
type DownloadService struct {
	baseCfg *config.AppConfig
	client  *geektime.Client
	pool    *pdf.BrowserPool
}

// NewDownloadService creates a DownloadService.
func NewDownloadService(baseCfg *config.AppConfig, client *geektime.Client, pool *pdf.BrowserPool) *DownloadService {
	return &DownloadService{baseCfg: baseCfg, client: client, pool: pool}
}

// SetClient updates the geektime client (e.g. after cookie refresh).
func (s *DownloadService) SetClient(client *geektime.Client) {
	s.client = client
}

// Client returns the current geektime client.
func (s *DownloadService) Client() *geektime.Client {
	return s.client
}

// LookupCourse fetches course metadata and validates access.
func (s *DownloadService) LookupCourse(ctx context.Context, req LookupRequest) (geektime.Course, error) {
	_ = ctx
	pt, ok := GetProductType(req.ProductType, req.Enterprise)
	if !ok {
		return geektime.Course{}, &apperr.APIError{
			Code: apperr.CodeBadRequest, Message: "unknown product_type",
			HTTPStatus: 400,
		}
	}
	if !pt.NeedSelectArticle {
		return geektime.Course{}, &apperr.APIError{
			Code: apperr.CodeBadRequest,
			Message: fmt.Sprintf("product_type %s uses single_video mode, not course lookup", pt.ID),
			HTTPStatus: 400,
		}
	}
	course, err := s.fetchCourse(pt, req.ProductID)
	if err != nil {
		return geektime.Course{}, err
	}
	if !course.Access {
		return geektime.Course{}, apperr.ErrNotPurchased
	}
	return course, nil
}

// LookupSingleVideoProduct returns title and article id for daily lesson style products.
func (s *DownloadService) LookupSingleVideoProduct(ctx context.Context, req LookupRequest) (title string, articleID int, err error) {
	_ = ctx
	pt, ok := GetProductType(req.ProductType, req.Enterprise)
	if !ok {
		return "", 0, &apperr.APIError{Code: apperr.CodeBadRequest, Message: "unknown product_type", HTTPStatus: 400}
	}
	productInfo, err := s.client.ProductInfo(req.ProductID)
	if err != nil {
		return "", 0, err
	}
	if productInfo.Data.Info.Extra.Sub.AccessMask == 0 {
		return "", 0, apperr.ErrNotPurchased
	}
	if !pt.validateProductCode(productInfo.Data.Info.Type) {
		return "", 0, apperr.ErrInvalidProduct
	}
	return productInfo.Data.Info.Title, productInfo.Data.Info.Article.ID, nil
}

// ExecuteDownload runs a download job synchronously (called by the worker).
func (s *DownloadService) ExecuteDownload(
	ctx context.Context,
	req DownloadRequest,
	reporter progress.Reporter,
) (course geektime.Course, downloadFolder string, err error) {
	pt, ok := GetProductType(req.ProductType, req.Enterprise)
	if !ok {
		return geektime.Course{}, "", &apperr.APIError{Code: apperr.CodeBadRequest, Message: "unknown product_type", HTTPStatus: 400}
	}
	cfg := mergeConfig(s.baseCfg, req.Options, req.Enterprise)
	downloader := coursedl.NewCourseDownloader(ctx, cfg, s.client, nil, reporter, s.pool)

	switch req.Mode {
	case "single_video":
		title, articleID, err := s.LookupSingleVideoProduct(ctx, LookupRequest{
			ProductType: req.ProductType,
			ProductID:   req.ProductID,
			Enterprise:  req.Enterprise,
		})
		if err != nil {
			return geektime.Course{}, "", err
		}
		columnDir, err := downloader.DownloadFolderForTitle(title)
		if err != nil {
			return geektime.Course{}, "", err
		}
		if reporter != nil {
			reporter.OnArticleStart(articleID, title, "downloading_video")
		}
		err = downloader.DownloadSingleVideoProduct(title, articleID, pt.SourceType)
		if err != nil {
			if reporter != nil {
				reporter.OnArticleFailed(articleID, err)
			}
			return geektime.Course{}, "", err
		}
		if reporter != nil {
			reporter.OnArticleComplete(articleID, nil)
		}
		return geektime.Course{ID: req.ProductID, Title: title}, columnDir, nil

	case "all", "articles":
		course, err = s.LookupCourse(ctx, LookupRequest{
			ProductType: req.ProductType,
			ProductID:   req.ProductID,
			Enterprise:  req.Enterprise,
		})
		if err != nil {
			return geektime.Course{}, "", err
		}
		uiOpt := pt.ToUIOption()
		if req.Mode == "all" {
			err = downloader.DownloadAll(course, uiOpt)
		} else {
			articles := filterArticles(course.Articles, req.ArticleIDs)
			if len(articles) == 0 {
				return geektime.Course{}, "", &apperr.APIError{
					Code: apperr.CodeBadRequest, Message: "no matching article_ids",
					HTTPStatus: 400,
				}
			}
			for _, a := range articles {
				if pt.isUniversity() {
					detail, derr := s.client.UniversityClassArticleDetail(course.ID, a.AID)
					if derr != nil {
						return course, "", derr
					}
					if detail.Data.VideoID == "" {
						if reporter != nil {
							reporter.OnArticleSkipped(a.AID, "训练营暂时只支持下载视频")
						}
						continue
					}
				}
				if err = downloader.DownloadArticle(course, uiOpt, a, req.Options.Overwrite); err != nil {
					return course, "", err
				}
			}
		}
		if err != nil {
			return course, "", err
		}
		folder, ferr := downloader.DownloadFolderForTitle(course.Title)
		return course, folder, ferr

	default:
		return geektime.Course{}, "", &apperr.APIError{
			Code: apperr.CodeBadRequest, Message: "mode must be all, articles, or single_video",
			HTTPStatus: 400,
		}
	}
}

func (s *DownloadService) fetchCourse(pt ProductType, productID int) (geektime.Course, error) {
	if pt.IsEnterpriseMode {
		return s.client.EnterpriseCourseInfo(productID)
	}
	if pt.isUniversity() {
		return s.client.UniversityClassInfo(productID)
	}
	course, err := s.client.CourseInfo(productID)
	if err != nil {
		return course, err
	}
	if !pt.validateProductCode(course.Type) {
		return course, apperr.ErrInvalidProduct
	}
	return course, nil
}

func filterArticles(articles []geektime.Article, aids []int) []geektime.Article {
	if len(aids) == 0 {
		return nil
	}
	want := make(map[int]struct{}, len(aids))
	for _, id := range aids {
		want[id] = struct{}{}
	}
	var out []geektime.Article
	for _, a := range articles {
		if _, ok := want[a.AID]; ok {
			out = append(out, a)
		}
	}
	return out
}

func mergeConfig(base *config.AppConfig, opts DownloadOptions, enterprise bool) *config.AppConfig {
	cfg := *base
	cfg.IsEnterprise = enterprise
	if opts.DownloadFolder != "" {
		cfg.DownloadFolder = opts.DownloadFolder
	}
	if opts.Quality != "" {
		cfg.Quality = opts.Quality
	}
	if opts.Output > 0 {
		cfg.ColumnOutputType = opts.Output
	}
	if opts.Comments >= 0 {
		cfg.DownloadComments = opts.Comments
	}
	if opts.Interval >= 0 {
		cfg.Interval = opts.Interval
	}
	if opts.PrintPDFWait > 0 {
		cfg.PrintPDFWaitSeconds = opts.PrintPDFWait
	}
	if opts.PrintPDFTimeout > 0 {
		cfg.PrintPDFTimeoutSeconds = opts.PrintPDFTimeout
	}
	return &cfg
}
