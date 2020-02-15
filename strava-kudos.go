// build for amd64
// GOOS=linux GOARCH=amd64 go build strava-kudos.go

package main

import (
	"github.com/joho/godotenv"
	"log"
	"os"
	"stravaKudos/bot"
	"stravaKudos/parser"
	"strconv"
	"time"
)

func init() {

	if err := godotenv.Load(); err != nil {
		log.Fatal("No .env file found")
	}
}

func main() {

	c := &parser.Client{}

	c.InitWebClient()

	c.SetUserAgent("Strava/33.0.0 (Linux; Android 8.0.0; Pixel 2 XL Build/OPD1.170816.004)")

	initDebug(c)

	s := &bot.Strava{}

	var scheme = "https"
	var siteDomain = scheme + "://m.strava.com"
	var langParam = "hl=en"

	s.MapUrls = map[string]string{
		"auth_url":      siteDomain + "/api/v3/oauth/internal/token?" + langParam,
		"my_profile":    siteDomain + "/api/v3/athlete?" + langParam,
		"followers_url": siteDomain + "/api/v3/athletes/{ATHLETE-ID}/followers?" + langParam,
		"feed_url":      siteDomain + "/api/v3/feed/athlete/{ATHLETE-ID}",
		"feed_param":    "?photo_sizes[]=240&single_entity_supported=true&modular=true&" + langParam,
		"kudos_url":     siteDomain + "/api/v3/activities/{ACTIVITIES-ID}/kudos?" + langParam,
	}

	s.ReadAuthToken()

	for {
		s.GetMyProfile(c)
		s.GetMyFollowers(c)

		c.ToLog("Followers => ", s.Followers)

		for _, followerId := range s.Followers {

			s.ParseAndKudosFollower(c, followerId)
		}

		c.ToLog(" THE END LOOP ")
		time.Sleep(2 * time.Hour)

	}

}

func initDebug(c *parser.Client) {
	debugString, DebugEnvExists := os.LookupEnv("DEBUG")

	if DebugEnvExists {
		debug, err := strconv.ParseBool(debugString)
		if err != nil {
			log.Panicf("func initDebug(): failed convert 'debugString' to bool : %s", err)
		}
		c.SetDebug(debug)
	}
}
