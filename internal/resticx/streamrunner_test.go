package resticx

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/alexzeitgeist/resticscope/internal/model"
)

// combinedRunner implements both Runner and StreamRunner, modeling a single
// fake (like the production ExecRunner) wired only as Client.Runner. It exercises
// the preserved fallback: when Stream is unset, streamRunner uses a Runner that
// also satisfies StreamRunner rather than failing.
type combinedRunner struct{}

func (*combinedRunner) Run(context.Context, []string, string, ...string) ([]byte, []byte, error) {
	return nil, nil, nil
}

func (*combinedRunner) RunStream(context.Context, []string, string, func(io.Reader) error, ...string) ([]byte, error) {
	return nil, nil
}

// TestStreamRunnerSelection pins the selection precedence of streamRunner: the
// Stream seam wins; a Runner that also implements StreamRunner is the preserved
// fallback; a buffered-only Runner or a zero Client yields ErrNoStreamRunner
// rather than a fabricated ExecRunner.
func TestStreamRunnerSelection(t *testing.T) {
	stream := &fakeStream{}
	combined := &combinedRunner{}
	buffered := &fakeRunner{}

	tests := []struct {
		name    string
		client  *Client
		want    StreamRunner // expected runner when no error
		wantErr bool
	}{
		{"stream seam used", &Client{Stream: stream}, stream, false},
		{"stream seam preferred over runner", &Client{Stream: stream, Runner: combined}, stream, false},
		{"runner implementing StreamRunner", &Client{Runner: combined}, combined, false},
		{"buffered-only runner errors", &Client{Runner: buffered}, nil, true},
		{"zero client errors", &Client{}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.client.streamRunner()
			if tt.wantErr {
				if !errors.Is(err, ErrNoStreamRunner) {
					t.Fatalf("want ErrNoStreamRunner, got %v", err)
				}
				if got != nil {
					t.Fatalf("want nil runner on error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("want runner %#v, got %#v", tt.want, got)
			}
		})
	}
}

// TestStreamingMethodsRequireStreamRunner checks that with only a buffered-only
// Runner wired, every streaming method returns ErrNoStreamRunner instead of
// spawning a real restic.
func TestStreamingMethodsRequireStreamRunner(t *testing.T) {
	c := &Client{Runner: &fakeRunner{}}
	creds := Creds{ResticPassword: "pw"}

	t.Run("StreamSnapshotTree", func(t *testing.T) {
		_, err := c.StreamSnapshotTree(t.Context(), testTarget, creds,
			"abcd", browseTimeout, func(model.BrowseNode) error { return nil })
		if !errors.Is(err, ErrNoStreamRunner) {
			t.Fatalf("want ErrNoStreamRunner, got %v", err)
		}
	})

	t.Run("StreamDiff", func(t *testing.T) {
		_, err := c.StreamDiff(t.Context(), testTarget, creds,
			"aa11bb22", "cc33dd44", time.Minute,
			func(model.DiffEntry) error { return nil }, nil)
		if !errors.Is(err, ErrNoStreamRunner) {
			t.Fatalf("want ErrNoStreamRunner, got %v", err)
		}
	})

	t.Run("ExtractTree", func(t *testing.T) {
		// Valid params so arg-validation (which intentionally wins over wiring)
		// does not short-circuit before the stream-runner check.
		err := c.ExtractTree(t.Context(), testTarget, creds,
			ExtractTreeParams{SnapshotID: testSnapID, Source: "/etc/nginx", Target: "/abs/staging"},
			func(ExtractTreeEvent) error { return nil })
		if !errors.Is(err, ErrNoStreamRunner) {
			t.Fatalf("want ErrNoStreamRunner, got %v", err)
		}
	})
}
