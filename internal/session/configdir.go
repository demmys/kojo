package session

import "github.com/loppo-llc/kojo/internal/configdir"

func configDirPath() string {
	return configdir.Path()
}
