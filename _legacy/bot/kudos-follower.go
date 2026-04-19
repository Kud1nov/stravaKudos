package bot

import (
	"log"
	"stravaKudos/parser"
	"strings"
)

func (s *Strava) kudosFollower(c *parser.Client, followerId string){
	var headers = map[string]string{}
	headers["authorization"] = "access_token " + s.authToken

	var myFolowersUrl = strings.ReplaceAll(s.MapUrls["kudos_url"], "{ACTIVITIES-ID}", followerId)

	_, statusCode := c.MakeRequest(myFolowersUrl, "POST", "", headers)

	if statusCode != 201 {
		log.Fatalf("Status from kudosFollower request no HTTP_CREATED | statusCode => %d", statusCode)
	}
}