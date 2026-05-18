package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// releaseSource is one place we can pull a signed release from. The
// order of the slice is the fallback order: the first reachable source
// wins, mirroring install.sh.
type releaseSource struct {
	name        string
	apiLatest   string
	releaseBase string
}

var updateSources = []releaseSource{
	{
		name:        "gitea",
		apiLatest:   "https://git.cer.sh/api/v1/repos/axodouble/quptime/releases/latest",
		releaseBase: "https://git.cer.sh/axodouble/quptime/releases/download",
	},
	{
		name:        "github",
		apiLatest:   "https://api.github.com/repos/Axodouble/QUptime/releases/latest",
		releaseBase: "https://github.com/Axodouble/QUptime/releases/download",
	},
}

func addUpdateCmd(root *cobra.Command) {
	var (
		checkOnly bool
		force     bool
		source    string
	)

	cmd := &cobra.Command{
		Use:   "update",
		Short: "Download and install the latest qu release in place",
		Long: `Replace the running qu binary with the latest published release.

Tries Gitea (the canonical home) first and falls back to GitHub on
failure — the same order install.sh uses. The downloaded binary is
verified against the published SHA256SUMS before it touches disk.

The replacement is atomic: the new binary is written next to the
current one and rename(2)'d into place. The currently-running process
continues with the old inode until it exits; the next invocation
picks up the new binary.

Linux only — no pre-built binaries are published for other platforms.

Run as the user that owns the binary (usually root for the default
/usr/local/bin install).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdate(cmd, updateOptions{
				checkOnly: checkOnly,
				force:     force,
				source:    source,
			})
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "report whether an update is available and exit; do not download or install")
	cmd.Flags().BoolVar(&force, "force", false, "reinstall even if already on the latest tag")
	cmd.Flags().StringVar(&source, "source", "", "restrict release source: gitea, github (default: try gitea, fall back to github)")
	root.AddCommand(cmd)
}

type updateOptions struct {
	checkOnly bool
	force     bool
	source    string
}

func runUpdate(cmd *cobra.Command, opts updateOptions) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("qu update only supports linux; got %s", runtime.GOOS)
	}
	arch, err := updateArch()
	if err != nil {
		return err
	}
	sources, err := pickSources(opts.source)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	current := cmd.Root().Version
	if current == "" {
		current = "dev"
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	rel, err := fetchLatestRelease(ctx, sources)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "current: %s\nlatest:  %s (source: %s)\n", current, rel.tag, rel.source.name)

	if !opts.force && sameVersion(current, rel.tag) {
		fmt.Fprintln(out, "already on the latest release; nothing to do (use --force to reinstall)")
		return nil
	}
	if opts.checkOnly {
		fmt.Fprintln(out, "update available; run `qu update` to install")
		return nil
	}

	exe, err := resolveSelf()
	if err != nil {
		return err
	}
	if err := assertCanReplace(exe); err != nil {
		return err
	}

	binaryName := fmt.Sprintf("qu-%s-linux-%s", rel.tag, arch)
	binURL := strings.TrimRight(rel.source.releaseBase, "/") + "/" + rel.tag + "/" + binaryName
	sumsURL := strings.TrimRight(rel.source.releaseBase, "/") + "/" + rel.tag + "/SHA256SUMS"

	fmt.Fprintf(out, "downloading %s\n", binURL)
	binBytes, err := httpGet(ctx, binURL)
	if err != nil {
		return fmt.Errorf("download binary: %w", err)
	}
	sumsBytes, err := httpGet(ctx, sumsURL)
	if err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}
	want, err := expectedSum(sumsBytes, binaryName)
	if err != nil {
		return err
	}
	got := sha256.Sum256(binBytes)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("checksum mismatch for %s — refusing to install", binaryName)
	}
	fmt.Fprintln(out, "checksum OK")

	if err := replaceBinary(exe, binBytes); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed %s to %s\n", rel.tag, exe)
	fmt.Fprintln(out, "note: any running qu serve still uses the old binary; restart it to pick up the new one")
	return nil
}

func updateArch() (string, error) {
	switch runtime.GOARCH {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture %s — pre-built binaries are published for amd64 and arm64 only; build from source", runtime.GOARCH)
	}
}

func pickSources(name string) ([]releaseSource, error) {
	if name == "" {
		return updateSources, nil
	}
	for _, s := range updateSources {
		if strings.EqualFold(s.name, name) {
			return []releaseSource{s}, nil
		}
	}
	names := make([]string, 0, len(updateSources))
	for _, s := range updateSources {
		names = append(names, s.name)
	}
	return nil, fmt.Errorf("unknown source %q (valid: %s)", name, strings.Join(names, ", "))
}

type resolvedRelease struct {
	tag    string
	source releaseSource
}

// fetchLatestRelease walks the source list and returns the first one
// whose /releases/latest endpoint hands back a usable tag. Stderr
// channels failures so the operator can see which source was tried.
func fetchLatestRelease(ctx context.Context, sources []releaseSource) (resolvedRelease, error) {
	var lastErr error
	for _, s := range sources {
		tag, err := fetchTag(ctx, s.apiLatest)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", s.name, err)
			continue
		}
		return resolvedRelease{tag: tag, source: s}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no sources configured")
	}
	return resolvedRelease{}, fmt.Errorf("no release source reachable: %w", lastErr)
}

func fetchTag(ctx context.Context, api string) (string, error) {
	body, err := httpGet(ctx, api)
	if err != nil {
		return "", err
	}
	// Gitea and GitHub both expose `.tag_name` on the latest-release
	// payload, so one decoder serves both.
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse release JSON: %w", err)
	}
	if payload.TagName == "" {
		return "", errors.New("empty tag_name in release response")
	}
	return payload.TagName, nil
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "qu-update")
	req.Header.Set("Accept", "application/octet-stream, application/json;q=0.9, */*;q=0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	// 64 MiB ceiling — the release binary is well under 30 MiB, the
	// JSON payload a few KiB. Caps memory if a source ever serves a
	// runaway response.
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// expectedSum pulls the hex sum for filename out of a SHA256SUMS file.
// The format is `<hex>  <name>` or `<hex> *<name>` (binary mode).
func expectedSum(sums []byte, filename string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == filename {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no SHA256SUMS entry for %s", filename)
}

// resolveSelf returns the canonical absolute path of the running
// binary, with symlinks evaluated so we replace the real file rather
// than the link in /usr/local/bin pointing at it.
func resolveSelf() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks for %s: %w", exe, err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("absolute path for %s: %w", resolved, err)
	}
	return abs, nil
}

// assertCanReplace bails out early if the operator is going to fail
// the rename at the end. We're checking the *directory* writability
// because that's what rename(2) actually needs — the binary file
// itself is replaced by directory-entry swap, not by writing to it.
func assertCanReplace(exe string) error {
	dir := filepath.Dir(exe)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Permission probe: try opening a tempfile in the target dir.
	// This naturally accounts for ACLs, read-only mounts, and any
	// other thing that would make rename fail later.
	probe, err := os.CreateTemp(dir, ".qu-update-probe-*")
	if err != nil {
		if os.IsPermission(err) || errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("cannot write to %s — re-run as the user that owns %s (try sudo)", dir, exe)
		}
		return fmt.Errorf("probe write access on %s: %w", dir, err)
	}
	probePath := probe.Name()
	probe.Close()
	os.Remove(probePath)
	return nil
}

// replaceBinary writes the new bytes to a sibling tempfile in the
// same directory, fsyncs, then renames over the existing binary.
// Same-directory rename keeps the swap on one filesystem so rename(2)
// is atomic; Linux is happy to replace a running executable because
// the kernel addresses the running process by inode, not pathname.
func replaceBinary(exe string, payload []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".qu-update-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if anything below fails before the rename.
	cleanup := func() { os.Remove(tmpPath) }
	defer func() {
		if cleanup != nil {
			cleanup()
		}
	}()

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp binary: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync temp binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp binary: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod temp binary: %w", err)
	}
	if err := os.Rename(tmpPath, exe); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, exe, err)
	}
	cleanup = nil
	return nil
}

// sameVersion compares the cobra version stamp against the upstream
// tag. The CLI's --version is typically the bare tag (`v0.1.2`) but
// could plausibly be set without the leading `v`, so we strip it on
// both sides before comparing. `dev` never matches a real tag.
func sameVersion(current, tag string) bool {
	if current == "" || current == "dev" {
		return false
	}
	return strings.TrimPrefix(current, "v") == strings.TrimPrefix(tag, "v")
}
