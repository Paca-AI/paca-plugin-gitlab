// Package main implements the com.paca.gitlab backend WASM plugin: the GitLab
// provider of Paca's Git provider SPI (spi_*.go). Core drives it through the
// internal-only POST /_git/:op routes; the plugin has no routes of its own, no
// per-project data and no frontend. Connections are GitLab access tokens
// (project, group or a bot user's personal token) kept encrypted here.
package main

import (
	plugin "github.com/Paca-AI/plugin-sdk-go"
	"github.com/Paca-AI/plugin-sdk-go/gitprovider/serve"
)

// gitlabPlugin implements plugin.Plugin.
type gitlabPlugin struct {
	db  *plugin.DB
	log *plugin.Logger
	cfg *plugin.Config
}

// Init mounts the provider operations.
func (p *gitlabPlugin) Init(ctx *plugin.Context) error {
	p.db = ctx.DB()
	p.log = ctx.Log()
	p.cfg = ctx.Config()
	serve.Register(ctx, newGLProvider(newDBStore(p)))
	return nil
}

// Shutdown is a no-op for this plugin.
func (p *gitlabPlugin) Shutdown() {}
