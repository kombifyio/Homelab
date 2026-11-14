package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kombifyio/techstack/internal/gocommon/nativeclient/interactiveauth"
	"github.com/kombifyio/techstack/pkg/agent/hostidentity"
	"github.com/kombifyio/techstack/pkg/config"
	"github.com/kombifyio/techstack/pkg/guardidentity"
)

const appliancePairCapability = "GUARD_APPLIANCE_PAIR=device-grant-v1"

// runAgentPair connects a freshly installed appliance using the existing native
// login, own-server pairing mint and host-key enrollment. No user session is saved.
func runAgentPair(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("techstack agent pair", flag.ContinueOnError)
	check := flags.Bool("check", false, "print the appliance pairing capability and exit")
	clientID := flags.String("client-id", os.Getenv(envCloudNativeClientID), "dedicated Auth0 native public client id")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *check {
		_, err := fmt.Fprintln(out, appliancePairCapability)
		return err
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("appliance pairing must run as root on the Linux appliance")
	}
	const directory = "/var/lib/kombify-appliance"
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	capability := filepath.Join(directory, "pairing")
	confirmed := filepath.Join(directory, "paired")
	if _, err := os.Stat(confirmed); err == nil {
		return runGuardLayout(ctx, []string{"apply", "--activate"})
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Before the first layout transfer, custody is still root's. A response
	// file alone does not prove confirmation; use the canonical identity state.
	manager, err := hostidentity.Open(resolveGuardStateDir(""))
	if err != nil {
		return err
	}
	if !manager.Bound(defaultEnrollmentFile) {
		if _, err := os.Stat(capability); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(hostidentity.LocateEnrollment(defaultEnrollmentFile, manager.Dir())); err == nil {
				return errors.New("an enrollment exists without a confirmed host key; detach that server before pairing again, or select local")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if strings.TrimSpace(*clientID) == "" {
				return errors.New("the installer has no Auth0 native client id; use a configured installer release")
			}
			provider, err := buildCloudNativeProvider(ctx, config.DefaultCloudAuthIssuer, *clientID, "https://api.kombify.io")
			if err != nil {
				return err
			}
			grant, err := interactiveauth.DeviceGrant(ctx, interactiveauth.DeviceGrantConfig{
				Provider: provider, Audience: "https://api.kombify.io", Timeout: 15 * time.Minute,
				Prompt: func(code, uri, _ string) {
					_, _ = fmt.Fprintf(out, "To sign in, open:\n  %s\nand enter the code:\n  %s\n", uri, code)
				},
			})
			if err != nil {
				return errors.New("account sign-in did not finish; retry pairing or keep using the appliance locally")
			}
			name, _ := os.Hostname()
			token, err := guardidentity.AppliancePairing(ctx, http.DefaultClient,
				"https://api.kombify.io/v1/techstack/trust/pairing-tokens", grant.AccessToken, name)
			grant.AccessToken, grant.RefreshToken, grant.IDToken = "", "", ""
			if err != nil {
				return err
			}
			// Keep only the short-lived capability across an interrupted enrollment.
			file, err := os.CreateTemp(directory, ".pairing-")
			if err != nil {
				return err
			}
			defer os.Remove(file.Name())
			_, writeErr := io.WriteString(file, token)
			syncErr := file.Sync()
			closeErr := file.Close()
			if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
				return errors.New("could not retain the one-time enrollment capability")
			}
			if err := os.Rename(file.Name(), capability); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := runAgentEnroll(ctx, []string{"--server=https://techstack.kombify.io", "--token-file=" + capability}, out); err != nil {
			return err
		}
		identity, err := hostidentity.Open(resolveGuardStateDir(""))
		if err != nil {
			return err
		}
		if !identity.Bound(defaultEnrollmentFile) {
			return errors.New("the control plane did not confirm a host-key identity; detach that server before pairing again, or select local")
		}
	}
	// Record confirmation before layout hands custody to the daemon user.
	// Power loss at any earlier step resumes the same canonical enrollment.
	removeSecretInput(capability)
	if err := os.WriteFile(confirmed, []byte("host-key enrollment confirmed\n"), 0600); err != nil {
		return err
	}
	return runGuardLayout(ctx, []string{"apply", "--activate"})
}
