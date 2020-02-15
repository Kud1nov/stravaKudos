package bot

import (
	"encoding/json"
	"stravaKudos/parser"
	"strconv"
)

func (s *Strava) GetMyProfile(c *parser.Client) (jsonData string) {

	var headers = map[string]string{}

	headers["authorization"] = "access_token " + s.authToken

	jsonData, statusCode := c.MakeRequest(s.MapUrls["my_profile"], "GET", "", headers)

	if statusCode == 401 {
		s.toAuth(c)
		jsonData = s.GetMyProfile(c)
	}

	var result map[string]interface{}

	err := json.Unmarshal([]byte(jsonData), &result)

	c.CheckError(err)

	if _, ok:= result["id"]; ok {

		s.athleteId = strconv.Itoa(int(result["id"].(float64)))
	}

	return
}