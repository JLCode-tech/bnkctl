// Package manifests exposes the embedded k8s manifest directories as an fs.FS.
package manifests

import "embed"

// FS is the embedded manifest filesystem across all cloud providers and BNK versions.
//
//go:embed all:addons all:azure-templates all:cert-manager all:gie all:host-device all:metrics-server all:multus all:nvidia-device-plugin all:shared all:sriov-external all:static all:templates
var FS embed.FS
