package cmd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/nicoxiang/geektime-downloader/internal/api"
	"github.com/nicoxiang/geektime-downloader/internal/auth"
	"github.com/nicoxiang/geektime-downloader/internal/config"
	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/pkg/logger"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

var serveCfg struct {
	addr   string
	apiKey string
	dbPath string
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start local HTTP API server for agent integrations",
	RunE: func(cmd *cobra.Command, args []string) error {
		if serveCfg.apiKey == "" {
			serveCfg.apiKey = os.Getenv("GEEKTIME_DL_API_KEY")
		}
		if serveCfg.apiKey == "" {
			return fmt.Errorf("--api-key or GEEKTIME_DL_API_KEY is required")
		}
		if serveCfg.dbPath == "" {
			configDir, err := os.UserConfigDir()
			if err != nil {
				return err
			}
			serveCfg.dbPath = filepath.Join(configDir, config.GeektimeDownloaderFolder, "jobs.db")
		}
		if err := os.MkdirAll(filepath.Dir(serveCfg.dbPath), 0o755); err != nil {
			return err
		}

		store, err := job.OpenStore(serveCfg.dbPath)
		if err != nil {
			return err
		}
		defer store.Close()

		if err := store.RecoverRunningJobs(cmd.Context()); err != nil {
			return err
		}

		sessionStore := auth.NewSQLiteSessionStore(store.DB())
		authMgr := auth.NewManager(sessionStore)
		if err := authMgr.Init(cmd.Context(), cfg.Gcid, cfg.Gcess); err != nil {
			return err
		}

		dlSvc := service.NewDownloadService(&cfg, authMgr.GetClient())
		worker := job.NewWorker(store, dlSvc, authMgr.GetClient, job.Stability{
			JobTimeout:        cfg.JobTimeout,
			HeartbeatTimeout:  cfg.HeartbeatTimeout,
			RateLimitCooldown: cfg.RateLimitCooldown,
		})
		worker.Start(cmd.Context())

		srv := api.NewServer(authMgr, store, worker, dlSvc, "dev", serveCfg.apiKey, worker.OnCookiesUpdated)
		httpServer := &http.Server{
			Addr:    serveCfg.addr,
			Handler: srv.Handler(),
		}
		logger.Infof("API server listening on http://%s", serveCfg.addr)
		return httpServer.ListenAndServe()
	},
}

func init() {
	serveCmd.Flags().StringVar(&serveCfg.addr, "addr", "127.0.0.1:8080", "HTTP listen address")
	serveCmd.Flags().StringVar(&serveCfg.apiKey, "api-key", "", "Bearer token for API authentication")
	serveCmd.Flags().StringVar(&serveCfg.dbPath, "db", "", "SQLite database path")
	rootCmd.AddCommand(serveCmd)
}
