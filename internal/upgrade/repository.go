package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Repository answers what the repository lacks for a DuckDB release: a HEAD of each extension's path as a node installs
// it (`<prefix>/<release>/<platform>/<name>.duckdb_extension.gz`), for every platform the fleet runs. A path not there
// is the answer ("" when every one is); a repository that does not answer now is the error, and the rollout waits.
func Repository(ctx context.Context, client *http.Client, prefix, release string, platforms, extensions []string) (
	string, error) {
	prefix = strings.TrimSuffix(prefix, "/")
	var missing []string
	for _, platform := range sorted(platforms) {
		for _, extension := range sorted(extensions) {
			url := prefix + "/" + release + "/" + platform + "/" + extension + ".duckdb_extension.gz"
			req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
			if err != nil {
				return "", err
			}
			resp, err := client.Do(req)
			if err != nil {
				return "", fmt.Errorf("%s does not answer: %w", prefix, err)
			}
			_ = resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusOK:
			case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode == http.StatusUnauthorized:
				missing = append(missing, extension+" for "+platform)
			default:
				return "", fmt.Errorf("%s answers %s for %s", prefix, resp.Status, url)
			}
		}
	}
	if len(missing) == 0 {
		return "", nil
	}
	return fmt.Sprintf("the repository %s serves no %s for DuckDB %s: put the builds into the channel (kista's "+
		"own administration), then pause and resume the rollout", prefix, strings.Join(missing, ", "), release), nil
}

// pathPart is what a platform or an extension's name may be in a path: a node reports them, and a part like `..`
// would ask the repository for another path
var pathPart = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

func sorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if pathPart.MatchString(s) && !strings.Contains(s, "..") && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
