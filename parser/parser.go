package parser

import (
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	userAgent    string
	debug        bool
	WebClient    *http.Client
	requestBody  io.Reader
	request      *http.Request
	response     *http.Response
	responseData string
	err          error
	url          *url.URL
}

func (c *Client) InitWebClient() {

	c.WebClient = &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableCompression:    true,
		},
	}
}

func (c *Client) MakeRequest(siteUrl string, method string, body string, headers map[string]string) (string, int) {

	var err error
	c.url, err = url.Parse(siteUrl)
	c.CheckError(err)

	if c.requestBody = nil; len(body) > 0 {
		c.requestBody = strings.NewReader(body)
	}

	c.request, err = http.NewRequest(method, c.url.String(), c.requestBody)
	c.CheckError(err)

	c.request.Header.Set("user-agent", c.userAgent)

	if len(headers) > 0 {
		for name, values := range headers {
			c.request.Header.Set(name, values)
		}
	}

	c.response, err = c.WebClient.Do(c.request)
	c.CheckError(err)

	if c.response != nil {
		if c.response.StatusCode == http.StatusOK {
			page, err := ioutil.ReadAll(c.response.Body)
			c.CheckError(err)
			c.responseData = string(page)
		}

		defer func() {
			c.CheckError(c.response.Body.Close())
		}()

	}

	return c.responseData, c.response.StatusCode
}
