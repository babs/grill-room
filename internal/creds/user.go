package creds

import "os/user"

func currentUser() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}
