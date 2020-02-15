package parser

import (
	"log"
)

func (c *Client) CheckError(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

func (c *Client) ToLog(v ...interface{}) {
	if c.debug {
		log.Println(v...)
	}
}

func (c *Client) SetUserAgent(userAgent string) {
	c.userAgent = userAgent
}

func (c *Client) SetDebug(debug bool) {
	c.debug = debug
}


