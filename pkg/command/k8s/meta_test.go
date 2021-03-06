package k8s_test

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"code.cloudfoundry.org/log-cache-cli/pkg/command/k8s"
)

var _ = Describe("Meta", func() {
	It("prints sources in ascending order by namespace, resource, and type", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, metaResponseInfo(
				"source-id-5",
				"a-source-id-6",
				"ns/pod/foo",
				"ns/deployment/foo",
				"ns/pod/bar",
				"",
				"ns2/statefulset/foo",
				"source-id-4",
				"source-id-2",
			))
		}))
		defer server.Close()
		var buf bytes.Buffer
		metaCmd := k8s.NewMeta(k8s.Config{
			Addr: server.URL,
		})
		metaCmd.SetOutput(&buf)
		metaCmd.SetArgs([]string{})

		err := metaCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(strings.Split(buf.String(), "\n")).To(Equal([]string{
			"RESOURCE        TYPE          NAMESPACE   COUNT    EXPIRED   CACHE DURATION",
			"                -             -           100000   85008     11m45s",
			"a-source-id-6   -             -           100000   85008     11m45s",
			"source-id-2     -             -           100000   85008     11m45s",
			"source-id-4     -             -           100000   85008     11m45s",
			"source-id-5     -             -           100000   99999     1s",
			"bar             pod           ns          100000   85008     11m45s",
			"foo             deployment    ns          100000   85008     11m45s",
			"foo             pod           ns          100000   85008     11m45s",
			"foo             statefulset   ns2         100000   85008     11m45s",
			"",
		}))
	})

	It("doesn't print anything if there is no data", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{}`)
		}))
		defer server.Close()
		var buf bytes.Buffer
		metaCmd := k8s.NewMeta(k8s.Config{
			Addr: server.URL,
		})
		metaCmd.SetOutput(&buf)
		metaCmd.SetArgs([]string{})

		err := metaCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(buf.String()).To(BeEmpty())
	})

	It("removes header when not writing to a tty", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, metaResponseInfo(
				"source-id-5",
				"ns/pod/foo",
				"source-id-4",
				"source-id-2",
			))
		}))
		defer server.Close()
		var buf bytes.Buffer
		metaCmd := k8s.NewMeta(k8s.Config{
			Addr: server.URL,
		}, k8s.WithMetaNoHeaders())
		metaCmd.SetOutput(&buf)
		metaCmd.SetArgs([]string{})

		err := metaCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(strings.Split(buf.String(), "\n")).To(Equal([]string{
			"source-id-2   -     -    100000   85008   11m45s",
			"source-id-4   -     -    100000   85008   11m45s",
			"source-id-5   -     -    100000   99999   1s",
			"foo           pod   ns   100000   85008   11m45s",
			"",
		}))
	})

	It("doesn't return an error if the server responds with no data", func() {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer server.Close()
		metaCmd := k8s.NewMeta(k8s.Config{
			Addr: server.URL,
		})
		var buf bytes.Buffer
		metaCmd.SetOutput(&buf)
		metaCmd.SetArgs([]string{})

		err := metaCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(buf.String()).To(BeEmpty())
	})

	It("timesout when server is taking too long", func() {
		done := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(time.Second):
			case <-done:
			}
		}))
		defer server.Close()
		metaCmd := k8s.NewMeta(k8s.Config{
			Addr: server.URL,
		}, k8s.WithMetaTimeout(time.Nanosecond))
		metaCmd.SetOutput(ioutil.Discard)
		metaCmd.SetArgs([]string{})

		var err error
		go func() {
			defer close(done)
			err = metaCmd.Execute()
		}()

		Eventually(done, "500ms").Should(BeClosed())
		Expect(err).To(MatchError(ContainSubstring("context deadline exceeded")))
	})
})

func metaResponseInfo(sourceIDs ...string) string {
	var metaInfos []string
	metaInfos = append(metaInfos, fmt.Sprintf(`"%s": {
	  "count": "100000",
	  "expired": "99999",
	  "oldestTimestamp": "1519256863100000000",
	  "newestTimestamp": "1519256863110000000"
	}`, sourceIDs[0]))
	for _, sourceID := range sourceIDs[1:] {
		metaInfos = append(metaInfos, fmt.Sprintf(`"%s": {
		  "count": "100000",
		  "expired": "85008",
		  "oldestTimestamp": "1519256157847077020",
		  "newestTimestamp": "1519256863126668345"
		}`, sourceID))
	}
	return fmt.Sprintf(`{ "meta": { %s }}`, strings.Join(metaInfos, ","))
}
