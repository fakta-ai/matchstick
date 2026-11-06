package github

import "time"

// AppJWT signs the short-lived JWT a GitHub App authenticates with, exactly as the client does: issued by the App's
// client ID, valid for ten minutes around now.
func AppJWT(c Credentials, now time.Time) (string, error) { return appToken(c, now) }
