package command

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pixil98/campaign-manager/internal/api"
	"github.com/pixil98/campaign-manager/internal/auth"
	"github.com/pixil98/campaign-manager/internal/authentik"
	"github.com/pixil98/campaign-manager/internal/campaign"
	"github.com/pixil98/campaign-manager/internal/dokuwiki"
	"github.com/pixil98/go-service"
)

// BuildWorkers assembles the API and the invitation reconciler from the
// config.
func BuildWorkers(config any) (service.WorkerList, error) {
	cfg, ok := config.(*Config)
	if !ok {
		return nil, errors.New("unable to cast config")
	}

	// Discovery fetches the issuer's keys now, so a wrong issuer stops the
	// service at start rather than failing every request.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	verifier, err := auth.NewVerifier(ctx, cfg.OIDC.Issuer, cfg.OIDC.ClientId)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}

	authentikToken, err := readToken(cfg.Authentik.Token, cfg.Authentik.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("authentik: %w", err)
	}
	wikiToken, err := readToken(cfg.Wiki.Token, cfg.Wiki.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("wiki: %w", err)
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	dir, err := authentik.New(cfg.Authentik.URL, authentikToken, hc)
	if err != nil {
		return nil, fmt.Errorf("authentik: %w", err)
	}
	wiki, err := dokuwiki.New(cfg.Wiki.URL, wikiToken, hc)
	if err != nil {
		return nil, fmt.Errorf("wiki: %w", err)
	}

	manager := campaign.NewManager(dir, wiki, campaign.Options{
		BaseGroup:           cfg.Authentik.BaseGroup,
		AuthentikURL:        cfg.Authentik.URL,
		InvitationFlow:      cfg.Authentik.InvitationFlow,
		InvitationLifetime:  time.Duration(cfg.Authentik.InvitationLifetime),
		SendInvitationEmail: cfg.Authentik.SendInvitationEmail,
		TemplatesNamespace:  cfg.Wiki.TemplatesNamespace,
	})

	return service.WorkerList{
		"api":        api.NewServer(cfg.Listen, verifier, manager),
		"reconciler": campaign.NewReconciler(manager, time.Duration(cfg.ReconcileInterval)),
	}, nil
}
