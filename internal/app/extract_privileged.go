package app

// The non-root side of privileged extraction sends validated requests and
// resolved credentials to the root helper over stdin. It maps wire events back
// to ordinary results and sentinels so only output ownership differs. The helper
// never runs secrets_command, and returned helper or runner errors remain path-free.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/alexzeitgeist/resticscope/internal/config"
	"github.com/alexzeitgeist/resticscope/internal/secrets"

	json "github.com/goccy/go-json"
)

// PrivilegedRunner provides root-helper execution and authorization.
type PrivilegedRunner interface {
	// Probe reports whether Run can proceed without interaction.
	Probe(ctx context.Context) error
	// Run sends payload over the helper's stdin, streams stdout events to onLine,
	// and returns when the helper finishes. It keeps stdin open for liveness and
	// closes it on cancellation.
	Run(ctx context.Context, payload []byte, onLine func(line []byte) error) error
	// AuthCommand returns an interactive command that pre-authorizes Run.
	AuthCommand() *exec.Cmd
}

// ErrPrivilegedExtractUnavailable indicates that no root helper is available.
var ErrPrivilegedExtractUnavailable = errors.New("extract: privileged extract not available")

// PrivilegedExtractProbe reports whether extraction can start without
// interactive authorization.
func (a *App) PrivilegedExtractProbe(ctx context.Context) error {
	if a.Priv == nil {
		return ErrPrivilegedExtractUnavailable
	}
	return a.Priv.Probe(ctx)
}

// PrivilegedAuthCommand returns the runner's interactive authorization command,
// or nil when unavailable, keeping elevation details out of the UI.
func (a *App) PrivilegedAuthCommand() *exec.Cmd {
	if a.Priv == nil {
		return nil
	}
	return a.Priv.AuthCommand()
}

// extractPrivileged delegates the restore pipeline to the root helper and
// rebuilds ordinary results and sentinels. The helper revalidates the parent-
// validated request at the privilege boundary.
func (a *App) extractPrivileged(ctx context.Context, req ExtractRequest, staging string, r config.Repo, material secrets.Material, onProgress func(ExtractProgress)) (ExtractResult, error) {
	var result ExtractResult
	if a.Priv == nil {
		return result, ErrPrivilegedExtractUnavailable
	}

	startedAt := a.Clock.Now()

	// The helper receives the effective target because it reads no config.
	helperReq := req
	if helperReq.TargetRoot == "" {
		helperReq.TargetRoot = a.Cfg.Extract.TargetRoot
	}
	payload, err := json.Marshal(helperPayload{
		Version:        helperPayloadVersion,
		Target:         targetOf(r),
		Creds:          resticCreds(material),
		Request:        helperReq,
		UnsafeSymlinks: a.Cfg.Extract.UnsafeSymlinks,
		// The helper shares only cache entries it can return to the user.
		CacheDir:       a.Cfg.Global.CacheDir,
		TimeoutSeconds: int64(a.Cfg.Extract.ExtractTimeout.Std().Seconds()),
	})
	if err != nil {
		return result, errors.New("extract: encode helper payload failed")
	}

	// Match the helper timeout and cancel across uid boundaries by closing stdin.
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
				helperResult = ev.Result
			}
		case helperEventError:
			if ev.Error != nil {
				helperFail = ev.Error
			}
		}
		return nil
	}
	runErr := a.Priv.Run(runCtx, payload, onLine)

	// Surface retained staging after the helper's best-effort ownership handoff.
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
		// Probe for possibly root-owned staging after a helper exits without an event.
		result.StagingDir, result.StagingCreated = probeHelperStaging(staging)
		return result, fmt.Errorf("extract: helper: %w", runErr)
	}
	if helperResult == nil {
		return result, errors.New("extract: helper returned no result")
	}

	result = *helperResult
	// Use the parent's clock across the helper boundary.
	result.Elapsed = a.Clock.Now().Sub(startedAt)
	a.logExtractSuccess(req, result)
	return result, nil
}

// helperSentinelError restores errors.Is identity from a wire code. Unknown
// codes return the helper's path-free message.
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
	case helperCodeInvalidRequest:
		return helperDetailedSentinel(ErrExtractInvalidRequest, msg)
	case helperCodeMetadataNorm:
		return helperDetailedSentinel(ErrExtractMetadataNormalization, msg)
	case helperCodeRenameFailed:
		return helperDetailedSentinel(ErrExtractRenameFailed, msg)
	}
	if msg == "" {
		msg = "extract: helper failed"
	}
	return errors.New(msg)
}

// helperDetailedSentinel preserves the helper's detail text while restoring
// errors.Is identity through Unwrap.
func helperDetailedSentinel(sentinel error, msg string) error {
	if msg == "" || msg == sentinel.Error() {
		return sentinel
	}
	return &helperSentinelDetailError{sentinel: sentinel, msg: msg}
}

type helperSentinelDetailError struct {
	sentinel error
	msg      string
}

func (e *helperSentinelDetailError) Error() string { return e.msg }
func (e *helperSentinelDetailError) Unwrap() error { return e.sentinel }

// probeHelperStaging checks staging after the helper exits without a terminal event.
func probeHelperStaging(staging string) (string, bool) {
	if _, err := os.Lstat(staging); err == nil {
		return staging, true
	}
	return "", false
}
