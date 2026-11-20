//go:build !linux

package main

import (
	"context"
	"github.com/kombifyio/homelab/pkg/agent/hostidentity"
)

func installHostedRILBootstrap(context.Context, string, string, hostidentity.EnrollResult) error {
	return nil
}
