package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	"github.com/sethgrid/pester"
)

// JoinRequest is the request to join a node
type JoinRequest struct {
	NodeID string `json:"nodeID"`
	Addr   string `json:"addr"`
}

// Validate validates the request
func (j *JoinRequest) Validate() error {

	if j.NodeID == "" {
		return fmt.Errorf("nodeID is empty")
	}

	_, err := net.DialTimeout("tcp", j.Addr, time.Second*3)
	if err != nil {
		return fmt.Errorf("invalid addr %v", err)
	}

	return nil
}

// ErrStatus sends a http error status
func ErrStatus(w http.ResponseWriter, r *http.Request, message string, statusCode int, err error) {
	var content []byte
	var e error

	content, e = ioutil.ReadAll(r.Body)
	if e != nil {
		glog.Error("ioutil.ReadAll failed")
	}

	glog.Errorf("msg %v, r.Body %v, err: %v", message, string(content), errors.Wrap(err, ""))

	http.Error(w, message, statusCode)
}

// RetryPost posts the value to a remote endpoint. also retries
func RetryPost(val interface{}, url string, retry int) int {

	b := new(bytes.Buffer)
	err := json.NewEncoder(b).Encode(val)
	if err != nil {
		glog.Errorf("http post bucket encoding failed. %v %v", err, url)
		return http.StatusInternalServerError
	}
	req, err := http.NewRequest("POST", url, b)
	if err != nil {
		glog.Errorf("http post rule bucket newrequest failed. %v %v", err, url)
		return http.StatusInternalServerError
	}
	req.Header.Add("Content-type", "application/json")

	client := pester.New()
	client.MaxRetries = retry
	resp, err := client.Do(req)
	if err != nil {
		glog.Errorf("http post rule bucket client.Do failed %v %v", err, url)
		return http.StatusInternalServerError
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		glog.Errorf("http post rule bucket unexpected status code %v %v", err, resp.StatusCode)
	}

	return resp.StatusCode
}
