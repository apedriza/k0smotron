/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"fmt"

	"github.com/k0sproject/k0smotron/internal/provisioner"
)

// downloadScript is the script used to download and install k0s on a node. It bassically mimics the official get.k0s.sh script
// but allows specifying the install path and architecture via environment variables.
// TODO: Use the official get.k0s.sh script when it supports these features.
// See: https://github.com/k0sproject/get/pull/22
const downloadScript = `#!/bin/sh
# vim:set filetype=sh:

set -e

_k0s_latest() {
  curl -sSLf "https://docs.k0sproject.io/stable.txt"
}

_detect_arch() {
  arch="$(uname -m)"
  case "$arch" in
    amd64|x86_64) echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    armv7l|armv8l|arm) echo "arm" ;;
    *) echo "Unsupported processor architecture: $arch" 1>&2; return 1 ;;
  esac
  unset arch
}

main() {
  : "${K0S_VERSION:=$(_k0s_latest)}"
  : "${K0S_INSTALL_PATH:=/usr/local/bin}"
  : "${K0S_ARCH:=$(_detect_arch)}"

  k0sBinary="k0s"
  k0sDownloadUrl="https://github.com/k0sproject/k0s/releases/download/$K0S_VERSION/$k0sBinary-$K0S_VERSION-$K0S_ARCH"
  mkdir -p -- "$K0S_INSTALL_PATH"
  curl -sSLf "$k0sDownloadUrl" >"$K0S_INSTALL_PATH/$k0sBinary"
  chmod 755 -- "$K0S_INSTALL_PATH/$k0sBinary"
}

main
`

// DownloadCommands constructs the download commands for a given URL and version.
func DownloadCommands(preInstalledK0s bool, url string, version string, k0sBinPath string) []string {
	if k0sBinPath != "" {
		return []string{
			fmt.Sprintf("K0S_INSTALL_PATH=%s sh /etc/download-k0s.sh", k0sBinPath),
		}
	}
	if preInstalledK0s {
		return nil
	}
	if url != "" {
		return []string{
			fmt.Sprintf("curl -sSfL --retry 5 %s -o /usr/local/bin/k0s", url),
			"chmod +x /usr/local/bin/k0s",
		}
	}

	if version != "" {
		return []string{
			fmt.Sprintf("curl -sSfL --retry 5 https://get.k0s.sh | K0S_VERSION=%s sh", version),
		}
	}

	// Default to k0s get script to download the latest version
	return []string{
		"curl -sSfL --retry 5 https://get.k0s.sh | sh",
	}
}

// GetDownloadK0sBinaryScript returns a provisioner.File that contains the script to download and install k0s.
func GetDownloadK0sBinaryScript() provisioner.File {
	return provisioner.File{
		Content:     downloadScript,
		Permissions: "0777",
		Path:        "/etc/download-k0s.sh",
	}
}
