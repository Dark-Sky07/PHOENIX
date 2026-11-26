package openvpn

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupEvidenceTest(t *testing.T, id int, email string) (string, string) {
	t.Helper()
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	dir := dataDirForID(id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ensureServerCertLocked(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensureClientCertLocked(dir, email); err != nil {
		t.Fatal(err)
	}
	generation, err := newDaemonGeneration()
	if err != nil {
		t.Fatal(err)
	}
	inst := Instance{Id: id, Port: 1194, Clients: []string{email}}
	if err := writeClientAllowlist(dir, inst.Clients); err != nil {
		t.Fatal(err)
	}
	if err := writeSessionEvidenceSetup(dir, inst, generation); err != nil {
		t.Fatal(err)
	}
	return dir, generation
}

func openSessionID(t *testing.T, dir string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, evidenceRoot, evidenceRecords, "*.open"))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected one admission record, got %d (%v)", len(files), err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	fields, err := parseSessionFields(data)
	if err != nil {
		t.Fatal(err)
	}
	return fields["session"]
}

func runHook(t *testing.T, dir, name string, env []string) error {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(dir, name))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%s: %s", name, out)
	}
	return err
}

func TestSessionEvidenceHookLifecycle(t *testing.T) {
	dir, generation := setupEvidenceTest(t, 741, "alice@example.test")
	env := []string{
		"common_name=alice@example.test",
		"trusted_ip=192.0.2.10",
		"trusted_port=4000",
	}
	if err := runHook(t, dir, "client-connect", env); err != nil {
		t.Fatalf("client-connect: %v", err)
	}
	session := openSessionID(t, dir)
	if !ActiveSessionEmails()["alice@example.test"] {
		t.Fatal("open session not visible to management suppression")
	}
	if sessions, err := CollectSessionEvidence(); err != nil || len(sessions) != 0 {
		t.Fatalf("unfinished session must not be billed: %v %v", sessions, err)
	}
	disconnectEnv := append(append([]string{}, env...), "bytes_received=1200", "bytes_sent=3400")
	if err := runHook(t, dir, "client-disconnect", disconnectEnv); err != nil {
		t.Fatalf("client-disconnect: %v", err)
	}
	sessions, err := CollectSessionEvidence()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("collect sessions: %v %v", sessions, err)
	}
	wantKey := "openvpn:741:" + generation + ":" + session
	if sessions[0].Key != wantKey || sessions[0].Rx != 1200 || sessions[0].Tx != 3400 || sessions[0].Email != "alice@example.test" {
		t.Fatalf("unexpected session evidence: %+v", sessions[0])
	}
	if err := AckSessionEvidence(sessions[0]); err != nil {
		t.Fatal(err)
	}
	if sessions, err := CollectSessionEvidence(); err != nil || len(sessions) != 0 {
		t.Fatalf("acknowledged evidence retained: %v %v", sessions, err)
	}
}

func TestSessionEvidenceFailClosed(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 742, "bob@example.test")
	admit := []string{
		"common_name=bob@example.test",
		"trusted_ip=192.0.2.11",
		"trusted_port=4001",
	}
	if err := runHook(t, dir, "client-connect", admit); err != nil {
		t.Fatal(err)
	}
	if err := runHook(t, dir, "client-disconnect", append(append([]string{}, admit...),
		"bytes_received=not-a-number", "bytes_sent=10")); err == nil {
		t.Fatal("invalid final counter accepted")
	}
	if err := CheckSessionEvidenceIncidents(); err == nil {
		t.Fatal("hook incident not visible")
	}
	if _, err := CollectSessionEvidence(); err == nil {
		t.Fatal("accounting proceeded with an unresolved incident")
	}
}

func TestTLSVerifierFailsClosed(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 743, "carol@example.test")
	crt, key := clientFiles(dir, "carol@example.test")
	cert, _, err := loadKeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	digest := certificateDigest(cert)
	verify := func(depth, digest string) error {
		t.Helper()
		cmd := exec.Command("sh", filepath.Join(dir, "tls-verify"), depth)
		cmd.Env = append(os.Environ(), "tls_digest_sha256_0="+digest)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("tls-verify: %s", out)
		}
		return err
	}
	if err := verify("0", digest); err != nil {
		t.Fatalf("known certificate rejected: %v", err)
	}
	// OpenVPN 2.7 exposes the same digest in lowercase; normalize and accept.
	if err := verify("0", strings.ToLower(digest)); err != nil {
		t.Fatalf("lowercase digest rejected: %v", err)
	}
	if err := verify("0", "00:11"); err == nil {
		t.Fatal("unknown certificate accepted")
	}
	if err := os.Remove(filepath.Join(dir, "client-allowlist")); err != nil {
		t.Fatal(err)
	}
	if err := verify("0", digest); err == nil {
		t.Fatal("missing allowlist accepted")
	}
	// Intermediate certificates do not carry the client digest; verification
	// of depth 0 is what enforces the client allowlist.
	if err := verify("1", ""); err != nil {
		t.Fatalf("intermediate verification should not require client digest: %v", err)
	}
	inst := Instance{Id: 743, Port: 1194, Clients: []string{"carol@example.test"}}
	if err := os.WriteFile(confPathForID(inst.Id), []byte(renderServerConf(inst, 1234)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := CheckSessionEvidenceIncidents(); err == nil {
		t.Fatal("missing configured allowlist did not raise a durable incident")
	}
	// Rendering the config also restores the panel-owned authorization file.
	if err := writeConfig(inst, 1234); err != nil {
		t.Fatal(err)
	}
	if err := verify("0", digest); err != nil {
		t.Fatalf("allowlist was not restored by writeConfig: %v", err)
	}
}

func TestServerConfigInstallsHooks(t *testing.T) {
	inst := Instance{Id: 744, Port: 1194, Clients: []string{"dave@example.test"}}
	conf := renderServerConf(inst, 12345)
	for _, want := range []string{
		"script-security 2\n", "tls-verify ", "client-connect ", "client-disconnect ",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("config missing %q:\n%s", want, conf)
		}
	}
}

func TestManagementDeltasAreSuppressedForHookedSessions(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 745, "alice@example.test")
	if err := runHook(t, dir, "client-connect", []string{
		"common_name=alice@example.test",
		"trusted_ip=203.0.113.1",
		"trusted_port=2222",
	}); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte(tabHeader + tabClient + "END\n"))
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	mgr := &Manager{procs: map[int]*managed{
		745: {mgmtPort: port, tag: "inbound-745"},
	}}
	inbounds, clients, err := mgr.CollectTrafficWithErrors()
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 0 {
		t.Fatalf("management deltas double-billed a hooked session: %+v", clients)
	}
	if len(inbounds) != 1 || inbounds[0].Up != 1234 || inbounds[0].Down != 5678 {
		t.Fatalf("inbound aggregate missing: %+v", inbounds)
	}
}

func TestStatusSurfacesDurableEvidenceWarnings(t *testing.T) {
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	m := &Manager{}
	if status := m.Status(); status.Error != "" {
		t.Fatalf("unexpected warning on clean spool: %q", status.Error)
	}
	session := strings.Repeat("cc", 16)
	writeOpenVPNFixture := func(name, data string) {
		t.Helper()
		path := filepath.Join(openvpnDir(), "748", evidenceRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeOpenVPNFixture("records/"+session+".open", "session="+session+"\ncommon_name=alice@example.test\ninbound=748\ngeneration="+strings.Repeat("a", 32)+"\n")
	if status := m.Status(); !strings.Contains(status.Error, "sessions without final usage evidence") {
		t.Fatalf("open session warning missing: %q", status.Error)
	}
	writeOpenVPNFixture("incidents/failure.1", "hook failed\n")
	if status := m.Status(); !strings.Contains(status.Error, "session evidence incident") {
		t.Fatalf("durable incident warning missing: %q", status.Error)
	}
}

func TestWriteConfigInstallsStableEvidenceGeneration(t *testing.T) {
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	inst := Instance{Id: 746, Port: 1194, Clients: []string{"erin@example.test"}}
	dir := dataDirForID(inst.Id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := ensureServerCertLocked(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensureClientCertLocked(dir, "erin@example.test"); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(inst, 1234); err != nil {
		t.Fatal(err)
	}
	first, err := loadOrCreateDaemonGeneration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !generationPattern.MatchString(first) {
		t.Fatalf("invalid generation %q", first)
	}
	// A config-only rerender must not invalidate evidence from this daemon.
	if err := writeConfig(inst, 1234); err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateDaemonGeneration(dir)
	if err != nil || second != first {
		t.Fatalf("generation changed without a daemon restart: %q -> %q (%v)", first, second, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "client-allowlist")); err != nil {
		t.Fatalf("config did not install authorization file: %v", err)
	}
	if err := CheckSessionEvidenceIncidents(); err != nil {
		t.Fatal(err)
	}
	rotated, err := rotateDaemonGeneration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rotated == first {
		t.Fatal("daemon restart did not rotate generation")
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon-generation"), []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateDaemonGeneration(dir); err == nil {
		t.Fatal("malformed generation was silently replaced")
	}
}

func TestSessionEvidenceUnmatchedDisconnectFailsClosed(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 747, "frank@example.test")
	env := []string{
		"common_name=frank@example.test",
		"trusted_ip=192.0.2.12",
		"trusted_port=4002",
		"bytes_received=100",
		"bytes_sent=200",
	}
	if err := runHook(t, dir, "client-disconnect", env); err == nil {
		t.Fatal("disconnect without an admission record was accepted")
	}
	if err := CheckSessionEvidenceIncidents(); err == nil {
		t.Fatal("unmatched disconnect left no incident")
	}
	if sessions, err := CollectSessionEvidence(); err == nil || len(sessions) != 0 {
		t.Fatalf("unmatched disconnect invented usage: %v %v", sessions, err)
	}
}

func TestSessionEvidenceDuplicateAdmissionFailsClosed(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 748, "grace@example.test")
	env := []string{
		"common_name=grace@example.test",
		"trusted_ip=192.0.2.13",
		"trusted_port=4003",
	}
	if err := runHook(t, dir, "client-connect", env); err != nil {
		t.Fatal(err)
	}
	if err := runHook(t, dir, "client-connect", env); err == nil {
		t.Fatal("duplicate active admission was accepted")
	}
	if err := CheckSessionEvidenceIncidents(); err == nil {
		t.Fatal("duplicate admission left no incident")
	}
}

func TestSessionEvidenceStagingAndRevocationFaultsFailClosed(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 749, "hana@example.test")
	records := filepath.Join(dir, evidenceRoot, evidenceRecords)
	staged := filepath.Join(records, ".connect.interrupted")
	if err := os.WriteFile(staged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if sessions, err := CollectSessionEvidence(); err == nil || len(sessions) != 0 {
		t.Fatalf("interrupted staging file was ignored: %v %v", sessions, err)
	}
	if err := CheckClientSessionEvidence([]string{"hana@example.test"}); err == nil {
		t.Fatal("interrupted staging file did not fence client operations")
	}
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	if sessions, err := CollectSessionEvidence(); err != nil || len(sessions) != 0 {
		t.Fatalf("cleanup did not restore evidence collection: %v %v", sessions, err)
	}

	inst := Instance{Id: 749, Port: 1194, Clients: []string{"hana@example.test"}}
	if err := os.WriteFile(confPathForID(inst.Id), []byte(renderServerConf(inst, 12345)), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client-revocations"), []byte("not-a-digest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSessionEvidenceIncidents(); err == nil {
		t.Fatal("malformed revocation state was treated as clear")
	}
}
