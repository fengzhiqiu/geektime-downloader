package service

import "github.com/nicoxiang/geektime-downloader/internal/ui"

const (
	ProductTypeColumn      = "column"
	ProductTypeDailyLesson = "daily_lesson"
	ProductTypeOpenCourse  = "open_course"
	ProductTypeQConPlus    = "qconplus"
	ProductTypeUniversity  = "university"
	ProductTypeOther       = "other"
)

// ProductType describes a downloadable geektime product category.
type ProductType struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	SourceType         int      `json:"source_type"`
	AcceptProductTypes []string `json:"accept_product_types"`
	NeedSelectArticle  bool     `json:"need_select_article"`
	IsEnterpriseMode   bool     `json:"enterprise"`
	uiIndex            int
}

// ListProductTypes returns product types available for the given enterprise mode.
func ListProductTypes(enterprise bool) []ProductType {
	if enterprise {
		return []ProductType{{
			ID:                 ProductTypeUniversity,
			Name:               "训练营",
			SourceType:         5,
			AcceptProductTypes: []string{"c44"},
			NeedSelectArticle:  true,
			IsEnterpriseMode:   true,
			uiIndex:            0,
		}}
	}
	return []ProductType{
		{ProductTypeColumn, "普通课程", 1, []string{"c1", "c3"}, true, false, 0},
		{ProductTypeDailyLesson, "每日一课", 2, []string{"d"}, false, false, 1},
		{ProductTypeOpenCourse, "公开课", 1, []string{"p35", "p29", "p30"}, true, false, 2},
		{ProductTypeQConPlus, "大厂案例", 4, []string{"q"}, false, false, 3},
		{ProductTypeUniversity, "训练营", 5, []string{""}, true, false, 4},
		{ProductTypeOther, "其他", 1, []string{"x", "c6"}, true, false, 5},
	}
}

// GetProductType returns a product type by id.
func GetProductType(id string, enterprise bool) (ProductType, bool) {
	for _, pt := range ListProductTypes(enterprise) {
		if pt.ID == id {
			return pt, true
		}
	}
	return ProductType{}, false
}

// ToUIOption converts to the legacy UI option used by CourseDownloader.
func (p ProductType) ToUIOption() ui.ProductTypeSelectOption {
	return ui.ProductTypeSelectOption{
		Index:              p.uiIndex,
		Text:               p.Name,
		SourceType:         p.SourceType,
		AcceptProductTypes: p.AcceptProductTypes,
		NeedSelectArticle:  p.NeedSelectArticle,
		IsEnterpriseMode:   p.IsEnterpriseMode,
	}
}

func (p ProductType) isUniversity() bool {
	return p.uiIndex == 4 && !p.IsEnterpriseMode
}

func (p ProductType) validateProductCode(productCode string) bool {
	for _, pt := range p.AcceptProductTypes {
		if pt == productCode {
			return true
		}
	}
	return false
}
