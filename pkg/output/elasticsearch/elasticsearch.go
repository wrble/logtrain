package elasticsearch

import (
	"encoding/base64"
	"errors"
	"github.com/trevorlinton/remote_syslog2/syslog"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Syslog creates a new syslog output to elasticsearch
type Syslog struct {
	akkeris  bool
	node     string
	index    string
	url      url.URL
	endpoint string
	client   *http.Client
	packets  chan syslog.Packet
	errors   chan<- error
	stop     chan struct{}
}

var syslogSchemas = []string{"elasticsearch://", "es://", "elasticsearch+https://", "elasticsearch+http://", "es+https://", "es+http://"}

func cleanString(str string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(str, "\"", "\\\""), "\n", "\\n"), "\r", "\\r"), "\x00", "")
}

// https://www.elastic.co/guide/en/elasticsearch/reference/current/docs-bulk.html

// Test the schema to see if its an elasticsearch schema
func Test(endpoint string) bool {
	for _, schema := range syslogSchemas {
		if strings.HasPrefix(strings.ToLower(endpoint), schema) == true {
			return true
		}
	}
	return false
}

func toURL(endpoint string) string {
	if strings.Contains(endpoint, "+https://") == true {
		return strings.Replace(strings.Replace(endpoint, "elasticsearch+https://", "https://", 1), "es+https://", "https://", 1)
	}
	if strings.Contains(endpoint, "+http://") == true {
		return strings.Replace(strings.Replace(endpoint, "elasticsearch+http://", "http://", 1), "es+http://", "http://", 1)
	}
	return strings.Replace(strings.Replace(endpoint, "elasticsearch://", "https://", 1), "es://", "https://", 1)
}

// Create a new elasticsearch endpoint
func Create(endpoint string, errorsCh chan<- error) (*Syslog, error) {
	if Test(endpoint) == false {
		return nil, errors.New("Invalid endpoint")
	}
	u, err := url.Parse(toURL(endpoint))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(u.Path, "/_bulk") == false {
		u.Path = u.Path + "/_bulk"
	}
	node := os.Getenv("NODE") // TODO: pass this into create
	if node == "" {
		node = "logtrain"
	}
	return &Syslog{
		node:     node,
		index:    u.Query().Get("index"),
		endpoint: endpoint,
		url:      *u,
		client:   &http.Client{},
		packets:  make(chan syslog.Packet, 10),
		errors:   errorsCh,
		stop:     make(chan struct{}, 1),
		akkeris:  os.Getenv("AKKERIS") == "true", // TODO: pass this in to Create for all outputs.
	}, nil
}

// Dial connects to an elasticsearch
func (log *Syslog) Dial() error {
	go log.loop()
	return nil
}

// Close closes the connection to elasticsearch
func (log *Syslog) Close() error {
	log.stop <- struct{}{}
	close(log.packets)
	return nil
}

// Pools returns whether the elasticsearch end point pools connections
func (log *Syslog) Pools() bool {
	return true
}

// Packets returns a channel to send syslog packets on
func (log *Syslog) Packets() chan syslog.Packet {
	return log.packets
}

func (log *Syslog) loop() {
	timer := time.NewTicker(time.Second)
	var payload string = ""
	var systemTags = ""
	if log.akkeris {
		systemTags = "\", \"akkeris\":\"true"
	}
	for {
		select {
		case p, ok := <-log.packets:
			if !ok {
				return
			}
			var index = log.index
			if index == "" {
				index = p.Hostname
			}
			payload += "{\"create\":{ \"_source\": \"logtrain\" \"_id\": \"" + strconv.Itoa(int(time.Now().Unix())) + "\", \"_index\": \"" + cleanString(index) + "\" }}\n" +
				"{ \"@timestamp\":\"" + p.Time.Format(syslog.Rfc5424time) +
				"\", \"hostname\":\"" + cleanString(p.Hostname) +
				"\", \"tag\":\"" + cleanString(p.Tag) +
				systemTags +
				"\", \"message\":\"" + cleanString(p.Message) +
				"\", \"severity\":" + strconv.Itoa(int(p.Severity)) +
				", \"facility\":" + strconv.Itoa(int(p.Facility)) + " }\n"
		case <-timer.C:
			if payload != "" {
				req, err := http.NewRequest(http.MethodPost, log.url.String(), strings.NewReader(string(payload)))
				if err != nil {
					log.errors <- err
				} else {
					req.Header.Set("content-type", "application/json")
					if pwd, ok := log.url.User.Password(); ok {
						if strings.ToLower(log.url.Query().Get("auth")) == "bearer" {
							req.Header.Set("Authorization", "Bearer "+pwd)
						} else if strings.ToLower(log.url.Query().Get("auth")) == "apikey" {
							req.Header.Set("Authorization", "ApiKey "+base64.StdEncoding.EncodeToString([]byte(log.url.User.Username()+":"+string(pwd))))
						} else {
							req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(log.url.User.Username()+":"+string(pwd))))
						}
					}
					resp, err := log.client.Do(req)
					if err != nil {
						log.errors <- err
					} else {
						body, err := ioutil.ReadAll(resp.Body)
						if err != nil {
							body = []byte{}
						}
						resp.Body.Close()
						if resp.StatusCode >= http.StatusMultipleChoices || resp.StatusCode < http.StatusOK {
							log.errors <- errors.New("invalid response from endpoint: " + resp.Status + " " + string(body) + "sent: [[ " + payload + " ]]")
						}
					}
				}
				payload = ""
			}
		case <-log.stop:
			return
		}
	}
}
