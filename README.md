# stravaKudos
The bot will monitor your followers every 2 hours and if it finds new activities will send liked it.

Bot uses [Strava API v3](https://developers.strava.com/docs/reference/).

## How to use

You need to edit in the `.env` file with your login details.

USER_EMAIL = `your email for Strava authorization`

USER_PASSWORD = `your password for Strava`

CLIENT_SECRET = `your client_secret`

client_secret you can get [here](https://www.strava.com/settings/api).

Сhange the paths to the binary files in the `supervisord.conf` file on your system and run supervisord
