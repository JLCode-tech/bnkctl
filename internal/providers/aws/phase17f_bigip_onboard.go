package aws

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/JLCode-tech/bnkctl/pkg/intent"
	"github.com/JLCode-tech/bnkctl/pkg/jumphost"
	"github.com/JLCode-tech/bnkctl/pkg/state"
)

// BIGIPBootPollInterval is the interval between SSH readiness probes while waiting for BIG-IP VE MCPD.
var BIGIPBootPollInterval = 15 * time.Second

// BIGIPBootPollTimeout is the maximum time to wait for BIG-IP VE to finish first-boot initialization.
var BIGIPBootPollTimeout = 12 * time.Minute

// JumphostSSHRunFunc is the seam used by Phase 17f to execute remote commands on the jumphost via EICE.
var JumphostSSHRunFunc = func(ctx context.Context, region, jumphostID, keyPath, remoteCmd string) (string, error) {
	return jumphost.SSHRunViaEICE(ctx, region, jumphostID, keyPath, remoteCmd)
}

// JumphostPushKeyFunc pushes a temporary SSH public key to the jumphost for EICE access.
var JumphostPushKeyFunc = func(ctx context.Context, region, jumphostID, pubKeyPath string) error {
	return jumphost.PushSSHPublicKey(ctx, region, jumphostID, pubKeyPath)
}

func generateBIGIPAdminPassword() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "Bnk1!" + hex.EncodeToString(b) + "Aa", nil
}

func BuildBIGIPOnboardScript(
	adminPassword string,
	extSelfIP string,
	localASN int,
	peerASN int,
	tmmExtSelfIP string,
	tier1VIP string,
	bnkListenerVIP string,
) string {
	return fmt.Sprintf(`set -e
tmsh modify auth user admin password '%s'
tmsh modify sys global-settings gui-setup disabled

if ! tmsh list net vlan external >/dev/null 2>&1; then
  tmsh create net vlan external interfaces replace-all-with { 1.1 } mtu 1500
fi

if ! tmsh list net self external-self >/dev/null 2>&1; then
  tmsh create net self external-self address %s/24 vlan external allow-service all
fi

tmsh modify net route-domain 0 routing-protocol replace-all-with { BGP }

imish -e "enable" \
  -e "configure terminal" \
  -e "router bgp %d" \
  -e "bgp router-id %s" \
  -e "bgp graceful-restart restart-time 120" \
  -e "neighbor %s remote-as %d" \
  -e "neighbor %s activate" \
  -e "end" \
  -e "write memory"

if ! tmsh list ltm pool bnk_gw_pool >/dev/null 2>&1; then
  tmsh create ltm pool bnk_gw_pool members replace-all-with { %s:80 } monitor http
else
  tmsh modify ltm pool bnk_gw_pool members replace-all-with { %s:80 } monitor http
fi

if ! tmsh list ltm virtual tier1_http_vs >/dev/null 2>&1; then
  tmsh create ltm virtual tier1_http_vs destination %s:80 ip-protocol tcp pool bnk_gw_pool profiles replace-all-with { tcp http } source-address-translation { type automap }
else
  tmsh modify ltm virtual tier1_http_vs destination %s:80 pool bnk_gw_pool
fi

tmsh save sys config
echo BIGIP_ONBOARD_OK
`,
		adminPassword,
		extSelfIP,
		localASN,
		extSelfIP,
		tmmExtSelfIP, peerASN,
		tmmExtSelfIP,
		bnkListenerVIP,
		bnkListenerVIP,
		tier1VIP,
		tier1VIP,
	)
}

func waitForBIGIPMCPD(ctx context.Context, region, jumphostID, eiceKeyPath, bigipKeyB64, mgmtIP string) error {
	deadline := time.Now().Add(BIGIPBootPollTimeout)
	probeCmd := fmt.Sprintf(
		`umask 077; echo '%s' | base64 -d > /tmp/.bigip.pem && chmod 600 /tmp/.bigip.pem && ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -i /tmp/.bigip.pem admin@%s "show sys mcp-state field-fmt" 2>/dev/null || ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -i /tmp/.bigip.pem root@%s "tmsh show sys mcp-state field-fmt" 2>/dev/null`,
		bigipKeyB64, mgmtIP, mgmtIP,
	)
	for {
		out, err := JumphostSSHRunFunc(ctx, region, jumphostID, eiceKeyPath, probeCmd)
		if err == nil && strings.Contains(out, "running") {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for BIG-IP VE (%s) mcpd running state: %w (out=%q)", mgmtIP, err, out)
			}
			return fmt.Errorf("timeout waiting for BIG-IP VE (%s) mcpd running state (last output: %q)", mgmtIP, out)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(BIGIPBootPollInterval):
		}
	}
}

func prepareEphemeralEICEKey(ctx context.Context, region, jumphostID string) (string, func(), error) {
	priv, pub, err := jumphost.GenerateEphemeralED25519()
	if err != nil {
		return "", func() {}, err
	}
	kf, err := os.CreateTemp("", "awsbnkctl-bigip-eice-*.key")
	if err != nil {
		return "", func() {}, err
	}
	_, _ = kf.Write(priv)
	_ = kf.Chmod(0o600)
	_ = kf.Close()

	pf, err := os.CreateTemp("", "awsbnkctl-bigip-eice-*.pub")
	if err != nil {
		_ = os.Remove(kf.Name())
		return "", func() {}, err
	}
	_, _ = pf.WriteString(pub)
	_ = pf.Close()

	cleanup := func() {
		_ = os.Remove(kf.Name())
		_ = os.Remove(pf.Name())
	}
	if err := JumphostPushKeyFunc(ctx, region, jumphostID, pf.Name()); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return kf.Name(), cleanup, nil
}

// Phase17fBIGIPOnboard configures the BIG-IP VE instance via SSH from the jumphost.
func Phase17fBIGIPOnboard(ctx context.Context, cl *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if cl == nil {
		return fmt.Errorf("phase 17f bigip onboard: cluster intent is nil")
	}
	if st == nil {
		return fmt.Errorf("phase 17f bigip onboard: state is nil")
	}
	if !cl.HasBIGIP() || dryRun {
		return nil
	}
	if st.Get("BIGIP_ONBOARDED") == "true" {
		return nil
	}

	mgmtIP := st.Get("BIGIP_MGMT_IP")
	extIP := st.Get("BIGIP_EXT_IP")
	tier1VIP := st.Get("BIGIP_VIP")
	jumphostID := st.Get("JUMPHOST_ID")
	jumphostKeyPath := st.Get("JUMPHOST_KEY_PATH")
	if mgmtIP == "" || extIP == "" || jumphostID == "" || jumphostKeyPath == "" {
		return fmt.Errorf("phase 17f bigip onboard: required state keys missing (BIGIP_MGMT_IP=%q BIGIP_EXT_IP=%q JUMPHOST_ID=%q JUMPHOST_KEY_PATH=%q)",
			mgmtIP, extIP, jumphostID, jumphostKeyPath)
	}

	// #nosec G304 -- jumphostKeyPath is workspace-managed jumphost key file
	keyBytes, err := os.ReadFile(jumphostKeyPath)
	if err != nil {
		return fmt.Errorf("phase 17f bigip onboard: reading %s: %w", jumphostKeyPath, err)
	}
	bigipKeyB64 := base64.StdEncoding.EncodeToString(keyBytes)

	adminPass := st.Get("BIGIP_ADMIN_PASSWORD")
	if adminPass == "" {
		if cl.BigIPVE != nil && cl.BigIPVE.AdminPassword != "" {
			adminPass = cl.BigIPVE.AdminPassword
		} else {
			adminPass, err = generateBIGIPAdminPassword()
			if err != nil {
				return fmt.Errorf("phase 17f bigip onboard: generating admin password: %w", err)
			}
		}
		st.Set("BIGIP_ADMIN_PASSWORD", adminPass)
		if err := st.Save(); err != nil {
			return err
		}
	}

	localASN := 65002
	peerASN := 65001
	if cl.BigIPVE != nil {
		if cl.BigIPVE.LocalASN > 0 {
			localASN = cl.BigIPVE.LocalASN
		}
		if cl.BigIPVE.PeerASN > 0 {
			peerASN = cl.BigIPVE.PeerASN
		}
	}

	var tmmExtSelfIP string
	if cl.Network.DataPath != nil && cl.Network.DataPath.SelfIPs != nil {
		tmmExtSelfIP = strings.Split(cl.Network.DataPath.SelfIPs.External, "/")[0]
	}
	bnkListenerVIP := st.Get("GATEWAY_VIP")
	if bnkListenerVIP == "" {
		bnkListenerVIP, _ = cl.DefaultVIP()
	}

	eiceKeyPath, cleanup, err := prepareEphemeralEICEKey(ctx, cl.Metadata.Region, jumphostID)
	if err != nil {
		return fmt.Errorf("phase 17f bigip onboard: preparing EICE key: %w", err)
	}
	defer cleanup()

	if err := waitForBIGIPMCPD(ctx, cl.Metadata.Region, jumphostID, eiceKeyPath, bigipKeyB64, mgmtIP); err != nil {
		return fmt.Errorf("phase 17f bigip onboard: %w", err)
	}

	eiceKeyPath2, cleanup2, err := prepareEphemeralEICEKey(ctx, cl.Metadata.Region, jumphostID)
	if err != nil {
		return fmt.Errorf("phase 17f bigip onboard: refreshing EICE key: %w", err)
	}
	defer cleanup2()

	script := BuildBIGIPOnboardScript(adminPass, extIP, localASN, peerASN, tmmExtSelfIP, tier1VIP, bnkListenerVIP)
	scriptB64 := base64.StdEncoding.EncodeToString([]byte(script))

	runCmd := fmt.Sprintf(
		`umask 077; echo '%s' | base64 -d > /tmp/.bigip.pem && chmod 600 /tmp/.bigip.pem && echo '%s' | base64 -d > /tmp/.bigip-onboard.sh && (ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i /tmp/.bigip.pem root@%s "bash -s" < /tmp/.bigip-onboard.sh || ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i /tmp/.bigip.pem admin@%s "bash -s" < /tmp/.bigip-onboard.sh) && rm -f /tmp/.bigip.pem /tmp/.bigip-onboard.sh`,
		bigipKeyB64, scriptB64, mgmtIP, mgmtIP,
	)
	out, err := JumphostSSHRunFunc(ctx, cl.Metadata.Region, jumphostID, eiceKeyPath2, runCmd)
	if err != nil || !strings.Contains(out, "BIGIP_ONBOARD_OK") {
		return fmt.Errorf("phase 17f bigip onboard: executing tmsh/imish onboarding on %s: err=%v out=%q", mgmtIP, err, out)
	}

	st.Set("BIGIP_ONBOARDED", "true")
	return st.Save()
}

// Phase17fBIGIPOnboardDown clears the BIGIP_ONBOARDED state key.
func Phase17fBIGIPOnboardDown(_ context.Context, _ *intent.Cluster, st *state.State, _ *Clients, dryRun bool) error {
	if st == nil || dryRun {
		return nil
	}
	st.Delete("BIGIP_ONBOARDED")
	return st.Save()
}
