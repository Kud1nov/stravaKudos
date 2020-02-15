package bot

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"stravaKudos/parser"
)

func (s *Strava) toAuth(c *parser.Client) {

	var headers = map[string]string{}

	headers["Content-Type"] = "application/json"

	email, EmailEnvExists := os.LookupEnv("USER_EMAIL")

	password, PasswordEnvExists := os.LookupEnv("USER_PASSWORD")

	clientSecret, ClientSecretEnvExists := os.LookupEnv("CLIENT_SECRET")

	if !EmailEnvExists {
		log.Fatal("USER_EMAIL no found in .env file")
	}

	if !PasswordEnvExists {
		log.Fatal("USER_PASSWORD no found in .env file")
	}

	if !ClientSecretEnvExists {
		log.Fatal("CLIENT_SECRET no found in .env file")
	}

	type AuthReqBody struct {
		ClientId     int    `json:"client_id,omitempty"`
		ClientSecret string `json:"client_secret,omitempty"`
		Email        string `json:"email,omitempty"`
		Password     string `json:"password,omitempty"`
	}

	authReqBody := &AuthReqBody{
		ClientId:     2,
		ClientSecret: clientSecret,
		Email:        email,
		Password:     password,
	}

	authReqBodyJson, err := json.Marshal(authReqBody)
	c.CheckError(err)

	html, statusCode := c.MakeRequest(s.MapUrls["auth_url"], "POST", string(authReqBodyJson), headers)

	if statusCode != 200 {
		log.Fatalf("Status from auth request no HTTP_OK | statusCode => %d", statusCode)
	}

	var result map[string]interface{}

	err = json.Unmarshal([]byte(html), &result)
	c.CheckError(err)

	if _, ok := result["access_token"]; ok {
		s.authToken = result["access_token"].(string)
		err = s.saveAuthToken()
		c.CheckError(err)
	}

}

func (s *Strava) ReadAuthToken() {

	tokenFile, tokenEnvExists := os.LookupEnv("AUTH_TOKEN")

	if tokenEnvExists {

		token, err := ioutil.ReadFile(tokenFile)

		if err != nil {
			log.Panicf("func readAuthToken(): failed reading data from tokenFile: %s", err)
		}

		s.authToken = string(token)
	}
}


func (s *Strava) saveAuthToken() (err error) {
	err = ioutil.WriteFile(os.Getenv("AUTH_TOKEN"), []byte(s.authToken), 0644)
	return
}