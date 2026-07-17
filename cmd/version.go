package cmd

// version is the build version. It is injected at release time by GoReleaser
// via the ldflag -X github.com/nicoxiang/geektime-downloader/cmd.version=...
// (see .goreleaser.yml). Defaults to "dev" for local/CI non-release builds.
var version = "dev"
