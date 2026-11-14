package advancedissuer_test

import (
	"github.com/kombifyio/techstack/internal/advancedissuer"
	"github.com/kombifyio/techstack/internal/executioncustody"
	"github.com/kombifyio/techstack/pkg/jobs"
)

// A native move dispatches only when the installation's issuer answers with the
// receive grant of each step: both Advanced issuer paths must satisfy the port.
var (
	_ jobs.TransferReceiveIssuer = (*advancedissuer.Issuer)(nil)
	_ jobs.TransferReceiveIssuer = (*executioncustody.Remote)(nil)
)
