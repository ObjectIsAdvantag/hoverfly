package main

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"

	log "github.com/Sirupsen/logrus"
	goproxy "gopkg.in/elazarl/goproxy.v1"
	"github.com/garyburd/redigo/redis"
	"io/ioutil"
)

type DBClient struct {
	cache Cache
	http  *http.Client
}

// request holds structure for request
type request struct {
	details requestDetails
}

var emptyResp = &http.Response{}

// requestDetails stores information about request, it's used for creating unique hash and also as a payload structure
type requestDetails struct {
	Path        string              `json:"path"`
	Method      string              `json:"method"`
	Destination string              `json:"destination"`
	Query       string              `json:"query"`
	Body        string              `json:"body"`
	RemoteAddr  string              `json:"remoteAddr"`
	Headers     map[string][]string `json:"headers"`
}

// hash returns unique hash key for request
func (r *request) hash() string {
	h := md5.New()
	io.WriteString(h, fmt.Sprintf("%s%s%s%s", r.details.Destination, r.details.Path, r.details.Method, r.details.Query))
	return fmt.Sprintf("%x", h.Sum(nil))
}

// res structure hold response body from external service, body is not decoded and is supposed
// to be bytes, however headers should provide all required information for later decoding
// by the client.
type response struct {
	Status  int                 `json:"status"`
	Body    string              `json:"body"`
	Headers map[string][]string `json:"headers"`
}

// Payload structure holds request and response structure
type Payload struct {
	Response response       `json:"response"`
	Request  requestDetails `json:"request"`
	ID       string         `json:"id"`
}

// recordRequest saves request for later playback
func (d *DBClient) captureRequest(req *http.Request) (*http.Response, error) {

	// this is mainly for testing, since when you create
	if req.Body == nil {
		req.Body = ioutil.NopCloser(bytes.NewBuffer([]byte("")))
	}

	reqBody, err := ioutil.ReadAll(req.Body)

	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
			"mode":  AppConfig.mode,
		}).Error("Got error when reading request body")
	}
	log.WithFields(log.Fields{
		"body": string(reqBody),
	}).Info("got request body")
	req.Body = ioutil.NopCloser(bytes.NewBuffer(reqBody))

	// forwarding request
	resp, err := d.doRequest(req)

	if err == nil {

		// getting response body
		respBody, err := httputil.DumpResponse(resp, true)
		if err != nil {
			// copying the response body did not work
			if err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
				}).Error("Failed to copy response body.")
			}
		}

		// saving response body with request/response meta to cache
		go d.save(req, resp, respBody, reqBody)
	}

	// return new response or error here
	return resp, err
}

// doRequest performs original request and returns response that should be returned to client and error (if there is one)
func (d *DBClient) doRequest(request *http.Request) (*http.Response, error) {
	// We can't have this set. And it only contains "/pkg/net/http/" anyway
	request.RequestURI = ""

	if AppConfig.middleware != "" {
		var payload Payload

		c := NewConstructor(request, payload)
		c.ApplyMiddleware(AppConfig.middleware)

		request = c.reconstructRequest()

	}

	resp, err := d.http.Do(request)

	if err != nil {
		log.WithFields(log.Fields{
			"error":  err.Error(),
			"host":   request.Host,
			"method": request.Method,
			"path":   request.URL.Path,
		}).Error("Could not forward request.")
		return nil, err
	}

	log.WithFields(log.Fields{
		"host":   request.Host,
		"method": request.Method,
		"path":   request.URL.Path,
	}).Info("Response got successfuly!")

	resp.Header.Set("hoverfly", "Was-Here")
	return resp, nil

}

// save gets request fingerprint, extracts request body, status code and headers, then saves it to cache
func (d *DBClient) save(req *http.Request, resp *http.Response, respBody []byte, reqBody []byte) {
	// record request here
	key := getRequestFingerprint(req)

	if resp == nil {
		resp = emptyResp
	} else {
		responseObj := response{
			Status:  resp.StatusCode,
			Body:    string(respBody),
			Headers: resp.Header,
		}

		log.WithFields(log.Fields{
			"path":          req.URL.Path,
			"rawQuery":      req.URL.RawQuery,
			"requestMethod": req.Method,
			"bodyLen":       len(reqBody),
			"destination":   req.Host,
			"hashKey":       key,
		}).Info("Capturing")

		requestObj := requestDetails{
			Path:        req.URL.Path,
			Method:      req.Method,
			Destination: req.Host,
			Query:       req.URL.RawQuery,
			Body:        string(reqBody),
			RemoteAddr:  req.RemoteAddr,
			Headers:     req.Header,
		}

		payload := Payload{
			Response: responseObj,
			Request:  requestObj,
			ID:       key,
		}
		// converting it to json bytes
		bts, err := json.Marshal(payload)

		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to marshal json")
		} else {
			d.cache.set(key, bts)
		}
	}

}

// getAllRecordsRaw returns raw (json string) for all records
func (d *DBClient) getAllRecordsRaw() ([]string, error) {
	keys, err := d.cache.getAllKeys()

	if err == nil {

		// checking if there are any records
		if len(keys) == 0 {
			return nil, nil
		}

		jsonStrs, err := d.cache.getAllValues(keys)

		if err != nil {
			log.WithFields(log.Fields{
				"error": err.Error(),
			}).Error("Failed to get all values (raw)")
			return nil, err
		} else {
			return jsonStrs, nil
		}

	} else {
		return nil, err
	}
}

// getAllRecords returns all stored
func (d *DBClient) getAllRecords() ([]Payload, error) {
	var payloads []Payload

	jsonStrs, err := d.getAllRecordsRaw()

	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Error("Failed to get all values")
	} else {

		if jsonStrs != nil {
			for _, v := range jsonStrs {
				var pl Payload
				err = json.Unmarshal([]byte(v), &pl)
				if err != nil {
					log.WithFields(log.Fields{
						"error": err.Error(),
						"json":  v,
					}).Warning("Failed to deserialize json")
				} else {
					payloads = append(payloads, pl)
				}
			}
		}
	}

	return payloads, err

}

// deleteAllRecords deletes all recorded requests
func (d *DBClient) deleteAllRecords() error {
	keys, err := d.cache.getAllKeys()

	if err != nil {
		log.WithFields(log.Fields{
			"error": err.Error(),
		}).Warning("Failed to get keys, cannot delete all records")
		return err
	} else {
		for _, v := range keys {
			err := d.cache.delete(v)
			if err != nil {
				log.WithFields(log.Fields{
					"error": err.Error(),
					"key":   v,
				}).Warning("Failed to delete key...")
			}
		}
		return nil
	}
}

// getRequestFingerprint returns request hash
func getRequestFingerprint(req *http.Request) string {
	details := requestDetails{Path: req.URL.Path, Method: req.Method, Destination: req.Host, Query: req.URL.RawQuery}
	r := request{details: details}
	return r.hash()
}

// getResponse returns stored response from cache
func (d *DBClient) getResponse(req *http.Request) *http.Response {

	key := getRequestFingerprint(req)
	var payload Payload

	payloadBts, err := redis.Bytes(d.cache.get(key))

	if err == nil {
		// getting cache response
		err = json.Unmarshal(payloadBts, &payload)
		if err != nil {
			log.Error(err)
			// what now?
		}

		c := NewConstructor(req, payload)

		if AppConfig.middleware != "" {
			_ = c.ApplyMiddleware(AppConfig.middleware)
		}

		response := c.reconstructResponse()

		log.WithFields(log.Fields{
			"key":        key,
			"status":     payload.Response.Status,
			"bodyLength": response.ContentLength,
			"mode":       AppConfig.mode,
		}).Info("Response found, returning")

		return response

	} else {
		log.WithFields(log.Fields{
			"error": err.Error(),
			"mode":  AppConfig.mode,
		}).Error("Failed to retrieve response from cache")
		// return error? if we return nil - proxy forwards request to original destination
		return goproxy.NewResponse(req,
			goproxy.ContentTypeText, http.StatusPreconditionFailed,
			"Coudldn't find recorded request, please record it first!")
	}

}

// modifyRequestResponse modifies outgoing request and then modifies incoming response, neither request nor response
// is saved to cache.
func (d *DBClient) modifyRequestResponse(req *http.Request, middleware string) (*http.Response, error) {

	// modifying request
	resp, err := d.doRequest(req)

	if err != nil {
		return nil, err
	}

	// preparing payload
	bodyBytes, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		log.WithFields(log.Fields{
			"error":      err.Error(),
			"middleware": middleware,
		}).Error("Failed to read response body after sending modified request")
		return nil, err
	}

	r := response{
		Status:  resp.StatusCode,
		Body:    string(bodyBytes),
		Headers: resp.Header,
	}
	payload := Payload{Response: r}

	c := NewConstructor(req, payload)
	// applying middleware to modify response
	err = c.ApplyMiddleware(middleware)

	if err != nil {
		return nil, err
	}

	newResponse := c.reconstructResponse()

	log.WithFields(log.Fields{
		"status":     newResponse.StatusCode,
		"middleware": middleware,
		"mode":       AppConfig.mode,
	}).Info("Response modified, returning")

	return newResponse, nil

}
