package elasticsearch

import (
	"encoding/base64"
	. "github.com/smartystreets/goconvey/convey"
	syslog2 "github.com/trevorlinton/remote_syslog2/syslog"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

type TestHttpMessage struct {
	Request *http.Request
	Body    string
}
type TestHttpServer struct {
	Incoming    chan TestHttpMessage
	ReturnError bool
}

func (hts *TestHttpServer) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	bytes, err := ioutil.ReadAll(req.Body)
	if err != nil {
		log.Fatalln(err)
	}
	req.Body.Close()
	if hts.ReturnError == true {
		res.WriteHeader(http.StatusInternalServerError)
		res.Write(([]byte)("ERROR"))
	} else {
		hts.Incoming <- TestHttpMessage{
			Request: req,
			Body:    string(bytes),
		}
		res.Write(([]byte)("OK"))
	}
}

func TestElasticsearchHttpOutput(t *testing.T) {
	errorCh := make(chan error, 1)
	testHttpServer := TestHttpServer{
		Incoming:    make(chan TestHttpMessage, 1),
		ReturnError: false,
	}
	s := &http.Server{
		Addr:           ":8083",
		Handler:        &testHttpServer,
		ReadTimeout:    10 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	syslog, err := Create("elasticsearch+http://user:pass@localhost:8083/tests", errorCh)
	go s.ListenAndServe()
	Convey("Ensure syslog is created", t, func() {
		So(err, ShouldBeNil)
	})

	Convey("Ensure we can start the http (application/syslog) syslog end point.", t, func() {
		So(syslog.Dial(), ShouldBeNil)
	})
	Convey("Ensure that an http transport explicitly pools connections.", t, func() {
		So(syslog.Pools(), ShouldEqual, true)
	})
	Convey("Ensure we can send syslog packets", t, func() {
		p := syslog2.Packet{
			Severity: 0,
			Facility: 0,
			Time:     time.Now(),
			Hostname: "localhost",
			Tag:      "HttpSyslogChannelTest",
			Message:  "Test Message",
		}
		syslog.Packets() <- p
		select {
		case message := <-testHttpServer.Incoming:
			So(message.Request.Header.Get("authorization"), ShouldEqual, "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")))
			So(message.Body, ShouldContainSubstring, "{ \"@timestamp\":\""+p.Time.Format(syslog2.Rfc5424time)+"\", \"hostname\":\""+p.Hostname+"\", \"tag\":\""+p.Tag+"\", \"message\":\""+p.Message+"\", \"severity\":0, \"facility\":0 }")
		case error := <-errorCh:
			log.Fatal(error.Error())
		}

	})
	Convey("Ensure we receive an error sending to a erroring endpoint", t, func() {
		testHttpServer.ReturnError = true
		syslog.Packets() <- syslog2.Packet{
			Severity: 0,
			Facility: 0,
			Time:     time.Now(),
			Hostname: "localhost",
			Tag:      "HttpSyslogChannelTest",
			Message:  "Failed Message That Shouldn't Happen",
		}
		select {
		case <-testHttpServer.Incoming:
			log.Fatal("No message should have been received from incoming...")
		case error := <-errorCh:
			So(error, ShouldNotBeNil)
		}
	})
	Convey("Ensure we can close a syslog end point...", t, func() {
		So(syslog.Close(), ShouldBeNil)
	})
	Convey("Test ApiKey Auth", t, func() {
		testHttpServer.ReturnError = false
		syslog, err := Create("elasticsearch+http://user:pass@localhost:8083/tests?auth=apikey", errorCh)
		So(err, ShouldBeNil)
		So(syslog.Dial(), ShouldBeNil)
		now := time.Now()
		p := syslog2.Packet{
			Severity: 0,
			Facility: 0,
			Time:     now,
			Hostname: "localhost",
			Tag:      "HttpSyslogChannelTest",
			Message:  "Test Message \"",
		}
		syslog.Packets() <- p
		select {
		case message := <-testHttpServer.Incoming:
			So(message.Request.Header.Get("authorization"), ShouldEqual, "ApiKey "+base64.StdEncoding.EncodeToString([]byte("user:pass")))
			So(message.Body, ShouldContainSubstring, "{ \"@timestamp\":\""+p.Time.Format(syslog2.Rfc5424time)+"\", \"hostname\":\""+p.Hostname+"\", \"tag\":\""+p.Tag+"\", \"message\":\""+strings.ReplaceAll(p.Message, "\"", "\\\"")+"\", \"severity\":0, \"facility\":0 }\n")
		case error := <-errorCh:
			log.Fatal(error.Error())
		}
		So(syslog.Close(), ShouldBeNil)
	})
	Convey("Test Bearer Auth", t, func() {
		testHttpServer.ReturnError = false
		syslog, err := Create("elasticsearch+http://:pass@localhost:8083/tests?auth=bearer", errorCh)
		So(err, ShouldBeNil)
		So(syslog.Dial(), ShouldBeNil)
		now := time.Now()
		p := syslog2.Packet{
			Severity: 0,
			Facility: 0,
			Time:     now,
			Hostname: "localhost",
			Tag:      "HttpSyslogChannelTest",
			Message:  "Test Message",
		}
		syslog.Packets() <- p
		select {
		case message := <-testHttpServer.Incoming:
			So(message.Request.Header.Get("authorization"), ShouldEqual, "Bearer pass")
			So(message.Body, ShouldContainSubstring, "{ \"@timestamp\":\""+p.Time.Format(syslog2.Rfc5424time)+"\", \"hostname\":\""+p.Hostname+"\", \"tag\":\""+p.Tag+"\", \"message\":\""+p.Message+"\", \"severity\":0, \"facility\":0 }\n")
		case error := <-errorCh:
			log.Fatal(error.Error())
		}
		So(syslog.Close(), ShouldBeNil)
	})
	Convey("Cleanup", t, func() {
		s.Close()
	})
}
