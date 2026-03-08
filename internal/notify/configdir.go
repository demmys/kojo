package notify

import "github.com/loppo-llc/kojo/internal/configdir"

func configDirPath() string {
	return configdir.Path()
}
