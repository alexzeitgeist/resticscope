package app

// extract_privileged.go is the parent (non-root) side of the privileged
// extract. App.Extract routes a req.Privileged request here; this file owns
// the same validation/cred-resolution preamble as the in-process path, then
// hands the pipeline to the root helper (extract_helper.go) through a
// PrivilegedRunner and maps the helper's wire events back onto the ordinary
// ExtractResult / sentinel-error surface — the TUI cannot tell the two paths
// apart except by the ownership of what lands on disk.
//
// Credentials are resolved HERE, in the user's own process and environment
// (secrets_command never runs under sudo), and ride to the helper inside the
// stdin payload. The errors returned here follow the same path-free contract
// as App.Extract: helper messages are path-free by construction, and runner
// failures are reduced to their first line of (path-free) sudo/helper stderr.

import (
	"context"
	"errors"
	"fmt"
	"os"

	json "github.com/goccy/go-json"
)

// PrivilegedRunner launches the extract helper as root. Production is
// *SudoPrivilegedRunner (sudo -n + self re-exec); tests inject a fake that
// replays scripted event lines. Declared consumer-side (rule 3).
type PrivilegedRunner interface {
	// Probe reports whether the helper can be launched right now without user
	// interaction (e.g. `sudo -n true`). The TUI uses a failed probe to run an
	// interactive `sudo -v` before retrying.
	Probe(ctx context.Context) error
	// Run launches the helper, delivers payload on its stdin (keeping the pipe
	// open afterwards as the liveness/cancel channel), and calls onLine with
	// each NDJSON event line from its stdout. It returns once the helper
	// finishes; ctx cancellation must close the stdin pipe so a root helper the
	// caller cannot signal still winds down.
	Run(ctx context.Context, payload []byte, onLine func(line []byte) error) error
}

// ErrPrivilegedExtractUnavailable is returned when no PrivilegedRunner is
// wired (non-TUI entry points) or the platform cannot support the helper.
var ErrPrivilegedExtractUnavailable = errors.New("extract: privileged extract not available")

// PrivilegedExtractProbe reports whether a privileged extract could start
// without interactive authentication. The TUI calls it on commit; a non-nil
// error means "run sudo -v first" (or, for ErrPrivilegedExtractUnavailable,
// "give up").
func (a *App) PrivilegedExtractProbe(ctx context.Context) error {
	if a.Priv == nil {
		return ErrPrivilegedExtractUnavailable
	}
	return a.Priv.Probe(ctx)
}

// extractPrivileged mirrors App.Extract's steps 1–4 in-process (fail fast,
// resolve creds with the user's environment), then delegates steps 5–9 to the
// root helper and reassembles its terminal event into the ordinary result /
// sentinel surface.
func (a *App) extractPrivileged(ctx context.Context, req ExtractRequest, onProgress func(ExtractProgress)) (ExtractResult, error) {
	var result ExtractResult
	if a.Priv == nil {
		return result, ErrPrivilegedExtractUnavailable
	}

	// 1+2. Validate request, gates — same order as App.Extract.
	staging, final, err := PlanExtractPaths(a.Cfg.Extract, req)
	if err != nil {
		return result, err
	}
	if err := checkExtractModeGate(req); err != nil {
		return result, err
	}
	if !liveExtractSupported {
		return result, errors.New("extract: not supported on this platform")
	}

	// 3. Cred resolution — in the user's process, with the user's environment.
	r, ok := a.repo(req.Repo)
	if !ok {
		return result, fmt.Errorf("extract: unknown repo %q", req.Repo)
	}
	if _, ok := a.Cfg.Credential(r.Credential); !ok {
		return result, fmt.Errorf("extract: credential %q not found", r.Credential)
	}
	material, err := a.Secrets.Resolve(r.Name, r.Credential)
	if err != nil {
		return result, err
	}

	// 4. Fresh-target check (the helper re-checks at the privilege boundary).
	if err := FreshTargetCheck(staging, final); err != nil {
		return result, err
	}

	startedAt := a.Clock.Now()

	// The helper reads no config file, so the request must carry the effective
	// target root explicitly.
	helperReq := req
	helperReq.Privileged = false
	if helperReq.TargetRoot == "" {
		helperReq.TargetRoot = a.Cfg.Extract.TargetRoot
	}
	payload, err := json.Marshal(helperPayload{
		Version:        helperPayloadVersion,
		Target:         targetOf(r),
		Creds:          resticCreds(material),
		Request:        helperReq,
		UnsafeSymlinks: a.Cfg.Extract.UnsafeSymlinks,
		TimeoutSeconds: int64(a.Cfg.Extract.ExtractTimeout.Std().Seconds()),
	})
	if err != nil {
		return result, errors.New("extract: encode helper payload failed")
	}

	// Per-op timeout, same bound the helper enforces; on expiry the runner
	// closes the helper's stdin, which is the cross-uid kill switch.
	runCtx, cancel := context.WithTimeout(ctx, a.Cfg.Extract.ExtractTimeout.Std())
	defer cancel()

	var helperResult *ExtractResult
	var helperFail *helperError
	onLine := func(line []byte) error {
		var ev helperEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return errors.New("extract: malformed helper event")
		}
		switch ev.Kind {
		case helperEventProgress:
			if ev.Progress != nil && onProgress != nil {
				onProgress(*ev.Progress)
			}
		case helperEventResult:
			if ev.Result != nil {
				res := *ev.Result
				helperResult = &res
			}
		case helperEventError:
			if ev.Error != nil {
				fail := *ev.Error
				helperFail = &fail
			}
		}
		return nil
	}
	runErr := a.Priv.Run(runCtx, payload, onLine)

	// The helper's terminal error event carries the staging fate (it has
	// already chown'd a retained staging tree back to the user); surface it so
	// the TUI can offer keep-or-delete exactly as for an in-process failure.
	if helperFail != nil {
		result.StagingDir = helperFail.StagingDir
		result.StagingCreated = helperFail.StagingCreated
	}

	// Prefer the local timeout/cancel verdict, mirroring App.Extract step 7.
	if ce := runCtx.Err(); ce != nil {
		if helperFail == nil {
			result.StagingDir, result.StagingCreated = probeHelperStaging(staging)
		}
		return result, fmt.Errorf("extract: %w", ce)
	}
	if helperFail != nil {
		return result, helperSentinelError(helperFail.Code, helperFail.Message)
	}
	if runErr != nil {
		// The helper died without a terminal event (sudo refused mid-run, crash).
		// Best-effort staging probe so output this run may own is not orphaned
		// silently — though a crashed helper's staging can be root-owned.
		result.StagingDir, result.StagingCreated = probeHelperStaging(staging)
		return result, fmt.Errorf("extract: helper: %w", runErr)
	}
	if helperResult == nil {
		return result, errors.New("extract: helper returned no result")
	}

	result = *helperResult
	// The parent's injected clock is authoritative for Elapsed, as in the
	// in-process path (the helper's wall-clock is a hostile-adjacent boundary).
	result.Elapsed = a.Clock.Now().Sub(startedAt)
	a.logExtractSuccess(req, result)
	return result, nil
}

// helperSentinelError reconstructs the app-layer sentinel matching a helper
// wire code, so errors.Is keeps working across the process boundary. Unknown
// codes surface the helper's (path-free) message verbatim.
func helperSentinelError(code, msg string) error {
	switch code {
	case helperCodeCanceled:
		return fmt.Errorf("extract: %w", context.Canceled)
	case helperCodeDeadline:
		return fmt.Errorf("extract: %w", context.DeadlineExceeded)
	case helperCodeStagingExists:
		return ErrExtractStagingExists
	case helperCodeFinalExists:
		return ErrExtractFinalExists
	}
	if msg == "" {
		msg = "extract: helper failed"
	}
	return errors.New(msg)
}

// probeHelperStaging reports whether the planned staging dir exists on disk —
// the fallback staging fate when the helper vanished without a terminal event.
func probeHelperStaging(staging string) (string, bool) {
	if _, err := os.Lstat(staging); err == nil {
		return staging, true
	}
	return "", false
}
