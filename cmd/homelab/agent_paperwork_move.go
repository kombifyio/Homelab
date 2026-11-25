//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/kombifyio/homelab/pkg/agent/hostidentity"
	"github.com/kombifyio/homelab/pkg/guardexec"
	"github.com/kombifyio/homelab/pkg/guardidentity"
)

// Only the node's privileged StackKits executor and the Guard user can reach
// this local purpose signer. The Paperwork container receives proofs over its
// parent's pipe; it receives neither this socket nor the Guard state directory.
func startGuardPaperworkMove(ctx context.Context, enrollment agentEnrollment, manager *hostidentity.Manager, log *slog.Logger) {
	if os.Getenv("STACKKIT_PAPERWORK_MOVE_ENABLED") != "true" || manager == nil {
		return
	}
	socket := os.Getenv("PAPERWORK_MOVE_GUARD_SOCKET")
	if socket == "" {
		socket = filepath.Join(filepath.Dir(guardexec.DefaultOperationsSocket), "paperwork-move.sock")
	}
	go func() {
		if err := servePaperworkMoveProofs(ctx, socket, enrollment, manager); err != nil {
			log.Warn("paperwork_move_proofs_unavailable")
		}
	}()
}
func servePaperworkMoveProofs(ctx context.Context, socket string, enrollment agentEnrollment, manager *hostidentity.Manager) error {
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("proof socket path is not a socket")
		}
		if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
			_ = conn.Close()
			return errors.New("proof socket already active")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	allow := operationsPeerPolicy()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		// Each bounded local request is small; serialization also keeps a local
		// caller from creating an unbounded set of signing goroutines.
		func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			uid, err := guardexec.PeerUID(conn)
			if err != nil || !allow(uid) {
				return
			}
			raw, err := io.ReadAll(io.LimitReader(conn, 8193))
			if err != nil || len(raw) > 8192 {
				return
			}
			var request struct {
				Kind string `json:"kind"`
				Node struct {
					SpaceID       string `json:"space_id"`
					SourceTenant  string `json:"source_tenant"`
					ConnectTenant string `json:"connect_tenant"`
					ServerID      string `json:"server_id"`
					WorkerID      string `json:"worker_id"`
					KeyThumbprint string `json:"guard_key_thumbprint"`
					Generation    int64  `json:"binding_generation"`
				} `json:"node"`
				Grant  string `json:"grant,omitempty"`
				Digest string `json:"request_sha256,omitempty"`
			}
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
				return
			}
			node := request.Node
			record := manager.Record()
			if node.ConnectTenant != enrollment.TenantID || node.ServerID != enrollment.ServerID || node.WorkerID != enrollment.RuntimeAgentID || node.KeyThumbprint != record.KeyThumbprint || node.Generation < 1 {
				return
			}
			in := guardidentity.HostProofInput{Issuer: node.WorkerID, Subject: node.ServerID, TenantID: node.ConnectTenant, Generation: node.Generation, Now: time.Now()}
			var proof string
			switch request.Kind {
			case "move":
				if request.Digest != "" {
					return
				}
				proof, err = manager.MintPaperworkMoveProof(in, request.Grant)
			case "pull":
				if request.Grant != "" {
					return
				}
				proof, err = manager.MintPaperworkPullProof(in, request.Digest)
			default:
				return
			}
			if err != nil {
				return
			}
			_ = json.NewEncoder(conn).Encode(struct {
				Proof string `json:"proof"`
			}{proof})
		}()
	}
}
