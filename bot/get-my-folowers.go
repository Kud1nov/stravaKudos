package bot

import (
	"encoding/json"
	"log"
	"stravaKudos/parser"
	"strconv"
	"strings"
)

func (s *Strava) GetMyFollowers(c *parser.Client){
	var headers = map[string]string{}

	headers["authorization"] = "access_token " + s.authToken

	var myFolowersUrl = strings.ReplaceAll(s.MapUrls["followers_url"], "{ATHLETE-ID}", s.athleteId)

	jsonData, statusCode := c.MakeRequest(myFolowersUrl, "GET", "", headers)

	if statusCode != 200 {
		log.Fatalf("Status from getMyFolowers request no HTTP_OK | statusCode => %d", statusCode)
	}

	var results []map[string]interface{}
	err := json.Unmarshal([]byte(jsonData), &results)
	c.CheckError(err)


	s.Followers = []string{}
	s.FollowersInfo = make(map[string]string)

	for _, result := range results {
		if _, ok:= result["id"]; ok {

			followersId := strconv.Itoa(int(result["id"].(float64)))

			username := result["firstname"].(string) + " " + result["lastname"].(string)

			s.Followers = append(s.Followers, followersId)

			s.FollowersInfo[followersId] = username
		}
	}
}