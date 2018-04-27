package command

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"code.cloudfoundry.org/cli/plugin"
	logcache "code.cloudfoundry.org/go-log-cache"
	"code.cloudfoundry.org/go-log-cache/rpc/logcache_v1"
	flags "github.com/jessevdk/go-flags"
)

type source struct {
	GUID string `json:"guid"`
	Name string `json:"name"`
}

type sourceInfo struct {
	Resources []source `json:"resources"`
}

type serviceInstance struct {
	Metadata struct {
		GUID string `json:"guid"`
	} `json:"metadata"`
	Entity struct {
		Name string `json:"name"`
	} `json:"entity"`
}

type servicesResponse struct {
	Resources []serviceInstance `json:"resources"`
}

type Tailer func(sourceID string, start, end time.Time) []string

type optionsFlags struct {
	Scope       string `long:"scope"`
	EnableNoise bool   `long:"noise"`
	ShowGUID    bool   `long:"guid"`
}

// Meta returns the metadata from Log Cache
func Meta(ctx context.Context, cli plugin.CliConnection, tailer Tailer, args []string, c HTTPClient, log Logger, tableWriter io.Writer) {
	opts := optionsFlags{
		Scope:       "all",
		EnableNoise: false,
		ShowGUID:    false,
	}

	args, err := flags.ParseArgs(&opts, args)
	if err != nil {
		log.Fatalf("Could not parse flags: %s", err)
	}

	if len(args) > 0 {
		log.Fatalf("Invalid arguments, expected 0, got %d.", len(args))
	}

	scope := strings.ToLower(opts.Scope)
	if invalidScope(scope) {
		log.Fatalf("Scope must be 'platform', 'applications' or 'all'.")
	}

	logCacheEndpoint, err := logCacheEndpoint(cli)
	if err != nil {
		log.Fatalf("Could not determine Log Cache endpoint: %s", err)
	}

	if strings.ToLower(os.Getenv("LOG_CACHE_SKIP_AUTH")) != "true" {
		token, err := cli.AccessToken()
		if err != nil {
			log.Fatalf("Unable to get Access Token: %s", err)
		}

		c = &tokenHTTPClient{
			c:           c,
			accessToken: token,
		}
	}

	client := logcache.NewClient(
		logCacheEndpoint,
		logcache.WithHTTPClient(c),
	)

	meta, err := client.Meta(ctx)
	if err != nil {
		log.Fatalf("Failed to read Meta information: %s", err)
	}

	resources, err := getSourceInfo(meta, cli)
	if err != nil {
		log.Fatalf("Failed to read application information: %s", err)
	}

	username, err := cli.Username()
	if err != nil {
		log.Fatalf("Could not get username: %s", err)
	}

	fmt.Fprintf(tableWriter, fmt.Sprintf(
		"Retrieving log cache metadata as %s...\n\n",
		username,
	))

	headerArgs := []interface{}{"Source", "Count", "Expired", "Cache Duration"}
	headerFormat := "%s\t%s\t%s\t%s\n"
	tableFormat := "%s\t%d\t%d\t%s\n"

	if opts.ShowGUID {
		headerArgs = append([]interface{}{"Source ID"}, headerArgs...)
		headerFormat = "%s\t" + headerFormat
		tableFormat = "%s\t" + tableFormat
	}

	if opts.EnableNoise {
		headerArgs = append(headerArgs, "Rate")
		headerFormat = strings.Replace(headerFormat, "\n", "\t%s\n", 1)
		tableFormat = strings.Replace(tableFormat, "\n", "\t%d\n", 1)
	}

	tw := tabwriter.NewWriter(tableWriter, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, headerFormat, headerArgs...)

	for _, app := range resources {
		m, ok := meta[app.GUID]
		if !ok {
			continue
		}
		delete(meta, app.GUID)
		if scope == "applications" || scope == "all" {
			args := []interface{}{app.Name, m.Count, m.Expired, cacheDuration(m)}
			if opts.ShowGUID {
				args = append([]interface{}{app.GUID}, args...)
			}
			if opts.EnableNoise {
				end := time.Now()
				start := end.Add(-time.Minute)
				args = append(args, len(tailer(app.GUID, start, end)))
			}

			fmt.Fprintf(tw, tableFormat, args...)
		}
	}

	idRegexp := regexp.MustCompile("[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

	// Apps that do not have a known name from CAPI
	if scope == "applications" || scope == "all" {
		for sourceID, m := range meta {
			if idRegexp.MatchString(sourceID) {
				args := []interface{}{sourceID, m.Count, m.Expired, cacheDuration(m)}
				if opts.ShowGUID {
					args = append([]interface{}{sourceID}, args...)
				}
				if opts.EnableNoise {
					end := time.Now()
					start := end.Add(-time.Minute)
					args = append(args, len(tailer(sourceID, start, end)))
				}
				fmt.Fprintf(tw, tableFormat, args...)
			}
		}
	}

	if scope == "platform" || scope == "all" {
		for sourceID, m := range meta {
			if !idRegexp.MatchString(sourceID) {
				args := []interface{}{sourceID, m.Count, m.Expired, cacheDuration(m)}
				if opts.ShowGUID {
					args = append([]interface{}{sourceID}, args...)
				}
				if opts.EnableNoise {
					end := time.Now()
					start := end.Add(-time.Minute)
					args = append(args, len(tailer(sourceID, start, end)))
				}

				fmt.Fprintf(tw, tableFormat, args...)
			}
		}
	}

	tw.Flush()
}

func getSourceInfo(metaInfo map[string]*logcache_v1.MetaInfo, cli plugin.CliConnection) ([]source, error) {
	var resources []source
	var sourceIDs []string

	meta := make(map[string]int)
	for k := range metaInfo {
		meta[k] = 1
		sourceIDs = append(sourceIDs, k)
	}

	for len(sourceIDs) > 0 {
		var r sourceInfo
		n := 50
		if len(sourceIDs) < 50 {
			n = len(sourceIDs)
		}

		lines, err := cli.CliCommandWithoutTerminalOutput(
			"curl",
			"/v3/apps?guids="+strings.Join(sourceIDs[0:n], ","),
		)
		if err != nil {
			return nil, err
		}

		sourceIDs = sourceIDs[n:]
		rb := strings.Join(lines, "")
		err = json.NewDecoder(strings.NewReader(rb)).Decode(&r)
		if err != nil {
			return nil, err
		}

		resources = append(resources, r.Resources...)
	}

	for _, res := range resources {
		delete(meta, res.GUID)
	}
	var s []string
	for id := range meta {
		s = append(s, id)
	}

	services, err := getServiceInfo(s, cli)
	if err != nil {
		return nil, err
	}
	resources = append(resources, services...)

	return resources, nil
}

func getServiceInfo(sourceIDs []string, cli plugin.CliConnection) ([]source, error) {
	var (
		responseBodies []string
		resources      []source
	)

	for len(sourceIDs) > 0 {
		n := 50
		if len(sourceIDs) < 50 {
			n = len(sourceIDs)
		}

		lines, err := cli.CliCommandWithoutTerminalOutput(
			"curl",
			"/v2/service_instances?guids="+strings.Join(sourceIDs[0:n], ","),
		)
		if err != nil {
			return nil, err
		}

		sourceIDs = sourceIDs[n:]
		responseBodies = append(responseBodies, strings.Join(lines, ""))
	}

	for _, rb := range responseBodies {
		var r servicesResponse
		err := json.NewDecoder(strings.NewReader(rb)).Decode(&r)
		if err != nil {
			return nil, err
		}
		for _, res := range r.Resources {
			resources = append(resources, source{
				GUID: res.Metadata.GUID,
				Name: res.Entity.Name,
			})
		}
	}

	return resources, nil
}

func cacheDuration(m *logcache_v1.MetaInfo) time.Duration {
	new := time.Unix(0, m.NewestTimestamp)
	old := time.Unix(0, m.OldestTimestamp)
	return new.Sub(old).Truncate(time.Second)
}

func truncate(count int, entries map[string]*logcache_v1.MetaInfo) map[string]*logcache_v1.MetaInfo {
	truncated := make(map[string]*logcache_v1.MetaInfo)
	for k, v := range entries {
		if len(truncated) >= count {
			break
		}
		truncated[k] = v
	}
	return truncated
}

func logCacheEndpoint(cli plugin.CliConnection) (string, error) {
	logCacheAddr := os.Getenv("LOG_CACHE_ADDR")

	if logCacheAddr != "" {
		return logCacheAddr, nil
	}

	apiEndpoint, err := cli.ApiEndpoint()
	if err != nil {
		return "", err
	}

	return strings.Replace(apiEndpoint, "api", "log-cache", 1), nil
}

func invalidScope(scope string) bool {
	validScopes := []string{"platform", "applications", "all"}

	if scope == "" {
		return false
	}

	for _, s := range validScopes {
		if scope == s {
			return false
		}
	}

	return true
}
