package control

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

// fakeHandler records the arguments it receives and returns canned results.
type fakeHandler struct {
	gotDir           string
	gotWithWorkspace bool
	gotTarget        string
	err              error
}

var fakeInfo = SessionInfo{ID: "abc123", Name: "anna-8cc7", RepoPath: "/repo", WorkDir: "/ws/anna-8cc7", Status: "idle"}

func (f *fakeHandler) New(dir string, withWorkspace bool) (SessionInfo, error) {
	f.gotDir, f.gotWithWorkspace = dir, withWorkspace
	return fakeInfo, f.err
}

func (f *fakeHandler) List() []SessionInfo { return []SessionInfo{fakeInfo} }

func (f *fakeHandler) Close(target string) (SessionInfo, error) {
	f.gotTarget = target
	return fakeInfo, f.err
}

// startServer listens in a short temp dir. macOS は Unix ソケットのパスが 104 バイトまでなので、
// t.TempDir()（テスト名を含む長いパス）でなく os.MkdirTemp("", ...) を使う。
func startServer(t *testing.T, h Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cdctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	Serve(ctx, ln, h)
	return dir
}

func TestCallRoundTrip(t *testing.T) {
	tests := []struct {
		name              string
		req               Request
		want              Response
		wantDir           string
		wantWithWorkspace bool
		wantTarget        string
	}{
		{
			name:              "new with workspace",
			req:               Request{Op: OpNew, Dir: "/repo/sub"},
			want:              Response{Session: &fakeInfo},
			wantDir:           "/repo/sub",
			wantWithWorkspace: true,
		},
		{
			name:              "new without workspace",
			req:               Request{Op: OpNew, Dir: "/repo", NoWorkspace: true},
			want:              Response{Session: &fakeInfo},
			wantDir:           "/repo",
			wantWithWorkspace: false,
		},
		{
			name: "list",
			req:  Request{Op: OpList},
			want: Response{Sessions: []SessionInfo{fakeInfo}},
		},
		{
			name:       "close",
			req:        Request{Op: OpClose, Target: "anna-8cc7"},
			want:       Response{Session: &fakeInfo},
			wantTarget: "anna-8cc7",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &fakeHandler{}
			dir := startServer(t, h)

			got, err := Call(dir, tt.req)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("response = %+v, want %+v", got, tt.want)
			}
			if h.gotDir != tt.wantDir || h.gotWithWorkspace != tt.wantWithWorkspace || h.gotTarget != tt.wantTarget {
				t.Errorf("handler got dir=%q withWorkspace=%v target=%q, want dir=%q withWorkspace=%v target=%q",
					h.gotDir, h.gotWithWorkspace, h.gotTarget, tt.wantDir, tt.wantWithWorkspace, tt.wantTarget)
			}
		})
	}
}

func TestCallReturnsHandlerError(t *testing.T) {
	tests := []struct {
		name    string
		req     Request
		wantErr string
	}{
		{name: "new fails", req: Request{Op: OpNew, Dir: "/repo"}, wantErr: "boom"},
		{name: "close fails", req: Request{Op: OpClose, Target: "x"}, wantErr: "boom"},
		{name: "unknown op", req: Request{Op: "restart"}, wantErr: `unknown op: "restart"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := startServer(t, &fakeHandler{err: errors.New("boom")})

			_, err := Call(dir, tt.req)
			if err == nil || err.Error() != tt.wantErr {
				t.Errorf("Call error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestCallWithoutServer(t *testing.T) {
	dir, err := os.MkdirTemp("", "cdctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	if _, err := Call(dir, Request{Op: OpList}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Call error = %v, want ErrNotRunning", err)
	}
}

func TestListRejectsSecondServer(t *testing.T) {
	dir := startServer(t, &fakeHandler{})

	if _, err := Listen(dir); !errors.Is(err, ErrAlreadyListening) {
		t.Errorf("second Listen error = %v, want ErrAlreadyListening", err)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "cdctl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// 異常終了したプロセスが残したソケットファイルを模す（誰も待ち受けていない）。
	if err := os.WriteFile(SocketPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := Listen(dir)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	ln.Close()
}
