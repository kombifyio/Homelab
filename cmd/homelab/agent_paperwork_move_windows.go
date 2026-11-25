//go:build windows

package main

import (
	"context"
	"log/slog"

	"github.com/kombifyio/homelab/pkg/agent/hostidentity"
)

func startGuardPaperworkMove(context.Context, agentEnrollment, *hostidentity.Manager, *slog.Logger) {}
