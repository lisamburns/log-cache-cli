package command_test

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"

	"code.cloudfoundry.org/log-cache-cli/pkg/command"
)

var _ = Describe("Tail", func() {
	It("writes results from server", func() {
		paths := make(chan string, 100)
		startTimes := make(chan string, 100)

		startTime := time.Now()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths <- r.URL.Path
			startTimes <- r.URL.Query().Get("start_time")
			fmt.Fprint(w, tailResponseBodyDesc(startTime))
		}))
		defer server.Close()
		var buf bytes.Buffer
		tailCmd := command.NewTail(command.Config{
			Addr: server.URL,
		})
		tailCmd.SetOutput(&buf)
		tailCmd.SetArgs([]string{"test-source-id"})

		err := tailCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Eventually(paths).Should(Receive(Equal("/v1/read/test-source-id")))
		Eventually(startTimes).Should(Receive(Or(Equal(""), Equal("0"))))
		Expect(strings.Split(buf.String(), "\n")).To(Equal([]string{
			"Retrieving logs for test-source-id...",
			"",
			fmt.Sprintf(logFormat, startTime.Format(timeFormat), "ERR"),
			fmt.Sprintf(logFormat, startTime.Add(1*time.Second).Format(timeFormat), "OUT"),
			fmt.Sprintf(logFormat, startTime.Add(2*time.Second).Format(timeFormat), "OUT"),
			"",
		}))
	})

	It("removes headers when not printing to a tty", func() {
		startTime := time.Now()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, tailResponseBodyDesc(startTime))
		}))
		defer server.Close()
		var buf bytes.Buffer
		tailCmd := command.NewTail(command.Config{
			Addr: server.URL,
		}, command.WithTailNoHeaders())
		tailCmd.SetOutput(&buf)
		tailCmd.SetArgs([]string{"test-source-id"})

		err := tailCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(strings.Split(buf.String(), "\n")).To(Equal([]string{
			fmt.Sprintf(logFormat, startTime.Format(timeFormat), "ERR"),
			fmt.Sprintf(logFormat, startTime.Add(1*time.Second).Format(timeFormat), "OUT"),
			fmt.Sprintf(logFormat, startTime.Add(2*time.Second).Format(timeFormat), "OUT"),
			"",
		}))
	})

	DescribeTable("returns an error if args are not correct", func(args []string) {
		tailCmd := command.NewTail(command.Config{})
		tailCmd.SetOutput(ioutil.Discard)
		tailCmd.SetArgs(args)

		err := tailCmd.Execute()

		Expect(err).To(HaveOccurred())
	},
		Entry("no source id", nil),
		Entry("too many args", []string{"foo", "bar"}),
	)

	It("returns an error when read fails", func() {
		tailCmd := command.NewTail(command.Config{
			Addr: "http://does-not-exist",
		})
		tailCmd.SetOutput(ioutil.Discard)
		tailCmd.SetArgs([]string{"test-source-id"})

		err := tailCmd.Execute()

		Expect(err).To(MatchError(ContainSubstring("no such host")))
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
		tailCmd := command.NewTail(command.Config{
			Addr: server.URL,
		}, command.WithTailTimeout(time.Nanosecond))
		tailCmd.SetOutput(ioutil.Discard)
		tailCmd.SetArgs([]string{"test-source-id"})

		var err error
		go func() {
			defer close(done)
			err = tailCmd.Execute()
		}()

		Eventually(done, "500ms").Should(BeClosed())
		Expect(err).To(MatchError(ContainSubstring("context deadline exceeded")))
	})

	DescribeTable("displays all event types", func(resp, result string) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, resp)
		}))
		defer server.Close()
		var buf bytes.Buffer
		tailCmd := command.NewTail(command.Config{
			Addr: server.URL,
		}, command.WithTailNoHeaders())
		tailCmd.SetOutput(&buf)
		tailCmd.SetArgs([]string{"test-source-id"})

		err := tailCmd.Execute()

		Expect(err).ToNot(HaveOccurred())
		Expect(strings.Split(buf.String(), "\n")).To(Equal([]string{
			result,
			"",
		}))
	},
		Entry(
			"log",
			logResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(logFormat, time.Unix(0, 1).Format(timeFormat), "OUT"),
		),
		Entry(
			"counter",
			counterResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(
				counterFormat,
				time.Unix(0, 1).Format(timeFormat),
				"some-name",
				99,
			),
		),
		Entry(
			"gauge",
			gaugeResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(
				gaugeFormat,
				time.Unix(0, 1).Format(timeFormat),
				"some-name",
				99.0,
				"my-unit",
				"some-other-name",
				101.0,
				"my-unit",
			),
		),
		Entry(
			"timer",
			timerResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(
				timerFormat,
				time.Unix(0, 1).Format(timeFormat),
				"1s",
			),
		),
		Entry(
			"event",
			eventResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(
				eventFormat,
				time.Unix(0, 1).Format(timeFormat),
				"some-title",
				"some-body",
			),
		),
		Entry(
			"unknown",
			unknownResponseBody(time.Unix(0, 1)),
			fmt.Sprintf(
				unknownFormat,
				time.Unix(0, 1).Format(timeFormat),
				`tags:<key:"foo" value:"bar" >`,
			),
		),
	)

	Describe("--follow", func() {
		It("streams data when follow flag is provided", func() {
			startTime := time.Now().Add(-1 * time.Minute)
			handler := newIncrementalHandler(
				tailResponseBodyDesc(startTime.Add(-30*time.Second)),
				tailResponseBodyAsc(startTime),
				tailResponseBodyAsc(startTime.Add(3*time.Second)),
			)
			server := httptest.NewServer(handler)
			defer server.Close()
			tailCmd := command.NewTail(command.Config{
				Addr: server.URL,
			}, command.WithTailTimeout(250*time.Millisecond))
			var buf bytes.Buffer
			tailCmd.SetArgs([]string{"--follow", "test-source-id"})
			tailCmd.SetOutput(&buf)

			err := tailCmd.Execute()

			Expect(err).ToNot(HaveOccurred())
			startTimeParam := handler.requests()[0].URL.Query().Get("start_time")
			Expect(startTimeParam).To(Or(Equal(""), Equal("0")))
			Expect(strings.Split(buf.String(), "\n")).To(ConsistOf(
				"Retrieving logs for test-source-id...",
				"",
				fmt.Sprintf(logFormat, startTime.Add(-30*time.Second).Format(timeFormat), "ERR"),
				fmt.Sprintf(logFormat, startTime.Add(-29*time.Second).Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Add(-28*time.Second).Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Add(1*time.Second).Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Add(2*time.Second).Format(timeFormat), "ERR"),
				fmt.Sprintf(logFormat, startTime.Add(3*time.Second).Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Add(4*time.Second).Format(timeFormat), "OUT"),
				fmt.Sprintf(logFormat, startTime.Add(5*time.Second).Format(timeFormat), "ERR"),
				"",
			))
		})

		It("returns an error when it can't write to output", func() {
			startTime := time.Now().Add(-1 * time.Minute)
			handler := newIncrementalHandler(
				tailResponseBodyAsc(startTime),
			)
			server := httptest.NewServer(handler)
			defer server.Close()
			tailCmd := command.NewTail(command.Config{
				Addr: server.URL,
			}, command.WithTailTimeout(250*time.Millisecond))
			tailCmd.SetArgs([]string{"--follow", "test-source-id"})
			tailCmd.SetOutput(errWriter{})

			err := tailCmd.Execute()

			Expect(err).To(HaveOccurred())
		})
	})
})

const (
	logFormat     = "%s [app-name/0] LOG/%s log body"
	counterFormat = "%s [app-name/0] COUNTER %s:%d"
	gaugeFormat   = "%s [app-name/0] GAUGE %s:%f %s %s:%f %s"
	timerFormat   = "%s [app-name/0] TIMER %s"
	eventFormat   = "%s [app-name/0] EVENT %s:%s"
	unknownFormat = "%s [app-name/0] UNKNOWN %s"
	timeFormat    = "2006-01-02T15:04:05.00-0700"

	responseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp":"%d",
					"log":{
						"payload":"bG9nIGJvZHkK"
					}
				},
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp":"%d",
					"log":{
						"payload":"bG9nIGJvZHkK"
					}
				},
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp":"%d",
					"log":{
						"payload":"bG9nIGJvZHkK",
						"type": "ERR"
					}
				}
			]
		}
	}`

	logResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp":"%d",
					"log":{
						"payload":"bG9nIGJvZHkK"
					}
				}
			]
		}
	}`

	counterResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp":"%d",
					"counter":{"name":"some-name","total":99}
				}
			]
		}
	}`

	gaugeResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp": "%d",
					"gauge": {
					  "metrics": {
						"some-name": {
						  "value": 99,
						  "unit":"my-unit"
						},
						"some-other-name": {
						  "value": 101,
						  "unit":"my-unit"
						}
					  }
					}
				  }
			]
		}
	}`

	timerResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"timestamp": "%d",
					"instance_id":"0",
					"timer": {
						"name": "http",
						"start": "%d",
						"stop": "%d"
					}
				}
			]
		}
	}`

	eventResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp": "%d",
					"event": {
						"title": "some-title",
						"body": "some-body"
					}
				}
			]
		}
	}`

	unknownResponseTemplate = `{
		"envelopes": {
			"batch": [
				{
					"source_id": "app-name",
					"instance_id":"0",
					"timestamp": "%d",
					"tags": {"foo":"bar"}
				}
			]
		}
	}`
)

func tailResponseBodyDesc(startTime time.Time) string {
	// NOTE: These are in descending order.
	return fmt.Sprintf(responseTemplate,
		startTime.Add(2*time.Second).UnixNano(),
		startTime.Add(1*time.Second).UnixNano(),
		startTime.UnixNano(),
	)
}

func tailResponseBodyAsc(startTime time.Time) string {
	// NOTE: These are in ascending order.
	return fmt.Sprintf(responseTemplate,
		startTime.UnixNano(),
		startTime.Add(1*time.Second).UnixNano(),
		startTime.Add(2*time.Second).UnixNano(),
	)
}

func logResponseBody(startTime time.Time) string {
	return fmt.Sprintf(logResponseTemplate, startTime.UnixNano())
}

func counterResponseBody(startTime time.Time) string {
	return fmt.Sprintf(counterResponseTemplate, startTime.UnixNano())
}

func gaugeResponseBody(startTime time.Time) string {
	return fmt.Sprintf(gaugeResponseTemplate, startTime.UnixNano())
}

func timerResponseBody(startTime time.Time) string {
	return fmt.Sprintf(
		timerResponseTemplate,
		startTime.UnixNano(),
		startTime.Add(1*time.Second).UnixNano(),
		startTime.Add(2*time.Second).UnixNano(),
	)
}

func eventResponseBody(startTime time.Time) string {
	return fmt.Sprintf(
		eventResponseTemplate,
		startTime.UnixNano(),
	)
}

func unknownResponseBody(startTime time.Time) string {
	return fmt.Sprintf(unknownResponseTemplate, startTime.UnixNano())
}

type incrementalHandler struct {
	mu        sync.Mutex
	count     int
	reqs      []*http.Request
	responses []string
}

func newIncrementalHandler(responses ...string) *incrementalHandler {
	return &incrementalHandler{
		responses: responses,
	}
}

func (i *incrementalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var resp string
	i.mu.Lock()
	if i.count < len(i.responses) {
		resp = i.responses[i.count]
	}
	i.count++
	i.reqs = append(i.reqs, r)
	i.mu.Unlock()
	fmt.Fprint(w, resp)
}

func (i *incrementalHandler) requests() []*http.Request {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reqs
}

type errWriter struct{}

func (e errWriter) Write(p []byte) (n int, err error) {
	return 0, errors.New("i am error")
}