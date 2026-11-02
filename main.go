// Command tw-sync runs Taskwarrior's S3 sync with credentials kept in the macOS
// Keychain. Taskwarrior can't use credential_process, so tw-sync reads the
// secrets and hands them to one `task sync` process through its environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

const (
	agentLabel    = "com.rscottlundgren.tw-sync"
	syncInterval  = 900
	syncTimeout   = 2 * time.Minute
	notifyAfter   = 5
	staleAfter    = 30 * 24 * time.Hour
	secretEnvName = "TW_SYNC_SECRET"
)

// Keychain account names, one per secret.
const (
	keyAccessKeyID = "aws-access-key-id"
	keySecretKey   = "aws-secret-access-key"
	keyEncryption  = "encryption-secret"
)

var secretKeys = []string{keyAccessKeyID, keySecretKey, keyEncryption}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "now":
		err = cmdNow()
	case "agent":
		err = cmdAgent(os.Args[2:])
	case "doctor":
		err = cmdDoctor()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tw-sync:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  tw-sync setup --device per|harvard --bucket NAME [--region us-east-1] [--allow-existing]
  tw-sync now
  tw-sync agent install|uninstall
  tw-sync doctor
`)
}

// keychainService lets tests use their own Keychain items.
func keychainService() string {
	if s := os.Getenv("TW_SYNC_KEYCHAIN_SERVICE"); s != "" {
		return s
	}
	return "tw-sync"
}

func keychainGet(account string) (string, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", keychainService(), "-a", account, "-w").Output()
	if err != nil {
		return "", fmt.Errorf("keychain item %q not readable: run tw-sync setup", account)
	}
	return strings.TrimSpace(string(out)), nil
}

// keychainSet sends the value over stdin so it never appears in a process list.
func keychainSet(account, value string) error {
	if strings.ContainsAny(value, "\"\\\n") {
		return fmt.Errorf("value for %q has characters tw-sync can't store", account)
	}
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(
		"add-generic-password -U -s %s -a %s -T /usr/bin/security -w \"%s\"\n",
		keychainService(), account, value))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("storing %q: %v: %s", account, err, scrub(string(out), []string{value}))
	}
	return nil
}

func taskBinary() (string, error) {
	for _, p := range []string{"/opt/homebrew/bin/task", "/usr/local/bin/task"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return exec.LookPath("task")
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	return h
}

func stateDir() string { return filepath.Join(home(), "Library/Application Support/tw-sync") }
func logDir() string   { return filepath.Join(home(), "Library/Logs/tw-sync") }
func rcPath() string   { return filepath.Join(home(), ".config/task/sync.rc") }

func taskrcPath() string {
	if p := os.Getenv("TASKRC"); p != "" {
		return p
	}
	return filepath.Join(home(), ".taskrc")
}

func dataDir() string {
	if p := os.Getenv("TASKDATA"); p != "" {
		return p
	}
	return filepath.Join(home(), ".task")
}

// syncRC holds no secrets. The encryption secret is an environment reference
// that is only set for the sync process, so a plain `task sync` stops with
// "sync.encryption_secret is required".
func syncRC(device, bucket, region string) string {
	recurrence := "off"
	if device == "per" {
		recurrence = "on"
	}
	return fmt.Sprintf(`# Managed by tw-sync. No secrets belong in this file.
sync.aws.region=%s
sync.aws.bucket=%s
sync.aws.default_credentials=true
sync.encryption_secret=$%s
recurrence=%s
`, region, bucket, secretEnvName, recurrence)
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	device := fs.String("device", "", "per or harvard")
	bucket := fs.String("bucket", "", "S3 bucket name")
	region := fs.String("region", "us-east-1", "S3 bucket region")
	allowExisting := fs.Bool("allow-existing", false, "connect a replica that already has tasks")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *device != "per" && *device != "harvard" {
		return errors.New("--device must be per or harvard")
	}
	if *bucket == "" {
		return errors.New("--bucket is required")
	}
	task, err := taskBinary()
	if err != nil {
		return errors.New("task is not installed")
	}

	count, err := taskCount(task)
	if err != nil {
		return err
	}
	if *device == "harvard" && count > 0 && !*allowExisting {
		return fmt.Errorf(`this replica has %d tasks, and a new replica must start empty.
Keep them by running:
  task export > ~/tasks-before-sync.json
  mv ~/.task ~/.task.before-sync
Then rerun setup, run tw-sync now, and bring them back with:
  task import ~/tasks-before-sync.json`, count)
	}
	if count > 0 {
		dir, err := backup(task)
		if err != nil {
			return fmt.Errorf("backup failed, nothing was changed: %w", err)
		}
		fmt.Println("Backed up", count, "tasks to", dir)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("setup needs a terminal for its hidden prompts")
	}
	prompts := map[string]string{
		keyAccessKeyID: "AWS access key ID",
		keySecretKey:   "AWS secret access key",
		keyEncryption:  "Encryption secret",
	}
	for _, k := range secretKeys {
		fmt.Printf("%s (hidden): ", prompts[k])
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return fmt.Errorf("%s was empty", prompts[k])
		}
		if err := keychainSet(k, v); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(filepath.Dir(rcPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(rcPath(), []byte(syncRC(*device, *bucket, *region)), 0o644); err != nil {
		return err
	}
	if err := ensureInclude(); err != nil {
		return err
	}
	fmt.Println("Setup complete. Next: tw-sync now, then tw-sync agent install")
	return nil
}

func taskCount(task string) (int, error) {
	out, err := exec.Command(task, "rc.verbose=nothing", "rc.gc=off", "rc.confirmation=off", "count").Output()
	if err != nil {
		return 0, fmt.Errorf("counting tasks: %w", err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// backup keeps a JSON export and a copy of the database, readable only by you.
func backup(task string) (string, error) {
	dir := filepath.Join(stateDir(), "backups", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out, err := exec.Command(task, "rc.verbose=nothing", "rc.gc=off", "export").Output()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), out, 0o600); err != nil {
		return "", err
	}
	db, err := os.ReadFile(filepath.Join(dataDir(), "taskchampion.sqlite3"))
	if err != nil {
		return "", err
	}
	return dir, os.WriteFile(filepath.Join(dir, "taskchampion.sqlite3"), db, 0o600)
}

func ensureInclude() error {
	line := "include " + rcPath()
	path := taskrcPath()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(data), line) {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n# Taskwarrior sync\n%s\n", line)
	return err
}

type state struct {
	LastSuccess time.Time `json:"last_success"`
	Failures    int       `json:"failures"`
}

func statePath() string { return filepath.Join(stateDir(), "state.json") }

func loadState() state {
	var s state
	if b, err := os.ReadFile(statePath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveState(s state) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0o600)
}

func cmdNow() error {
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(stateDir(), "sync.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Println("A sync is already running. Skipping.")
		return nil
	}

	output, syncErr := runSync()
	s := loadState()
	if syncErr != nil {
		s.Failures++
		if s.Failures == notifyAfter {
			notify(fmt.Sprintf("Taskwarrior sync has failed %d times in a row.", s.Failures))
		}
	} else {
		s = state{LastSuccess: time.Now()}
	}
	if err := saveState(s); err != nil {
		return err
	}
	logResult(output, syncErr)
	fmt.Print(output)
	return syncErr
}

func runSync() (string, error) {
	task, err := taskBinary()
	if err != nil {
		return "", errors.New("task is not installed")
	}
	secrets := make([]string, len(secretKeys))
	for i, k := range secretKeys {
		if secrets[i], err = keychainGet(k); err != nil {
			return "", err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, task, "rc.confirmation=off", "sync")
	cmd.Env = append(cleanEnv(os.Environ()),
		"AWS_ACCESS_KEY_ID="+secrets[0],
		"AWS_SECRET_ACCESS_KEY="+secrets[1],
		secretEnvName+"="+secrets[2])
	out, err := cmd.CombinedOutput()
	output := scrub(string(out), secrets)
	if ctx.Err() != nil {
		return output, fmt.Errorf("sync timed out after %s", syncTimeout)
	}
	if err != nil {
		return output, fmt.Errorf("task sync failed: %w", err)
	}
	return output, nil
}

// cleanEnv drops any AWS settings that could override the sync user's keys.
func cleanEnv(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "AWS_") || strings.HasPrefix(e, secretEnvName+"=") {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

func scrub(text string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "<redacted>")
		}
	}
	return text
}

func logResult(output string, syncErr error) {
	if err := os.MkdirAll(logDir(), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(logDir(), "sync.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	result := "ok"
	if syncErr != nil {
		result = "FAILED: " + syncErr.Error()
	}
	fmt.Fprintf(f, "%s %s\n%s\n", time.Now().Format(time.RFC3339), result, strings.TrimSpace(output))
}

func notify(message string) {
	script := fmt.Sprintf("display notification %q with title %q", message, "tw-sync")
	_ = exec.Command("/usr/bin/osascript", "-e", script).Run()
}

func plistPath() string {
	return filepath.Join(home(), "Library/LaunchAgents", agentLabel+".plist")
}

// agentPlist passes proxy settings through, because launchd jobs don't inherit
// a shell's environment.
func agentPlist(exe string) string {
	var proxy strings.Builder
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if v := os.Getenv(name); v != "" {
			fmt.Fprintf(&proxy, "    <key>%s</key><string>%s</string>\n", name, v)
		}
	}
	env := ""
	if proxy.Len() > 0 {
		env = "  <key>EnvironmentVariables</key>\n  <dict>\n" + proxy.String() + "  </dict>\n"
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>now</string></array>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
%s</dict>
</plist>
`, agentLabel, exe, syncInterval,
		filepath.Join(logDir(), "agent.log"), filepath.Join(logDir(), "agent.log"), env)
}

func cmdAgent(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: tw-sync agent install|uninstall")
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	switch args[0] {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(logDir(), 0o700); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(plistPath(), []byte(agentPlist(exe)), 0o644); err != nil {
			return err
		}
		_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+agentLabel).Run()
		if out, err := exec.Command("/bin/launchctl", "bootstrap", domain, plistPath()).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
		}
		fmt.Printf("Agent installed. It syncs every %d minutes.\n", syncInterval/60)
	case "uninstall":
		_ = exec.Command("/bin/launchctl", "bootout", domain+"/"+agentLabel).Run()
		if err := os.Remove(plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		fmt.Println("Agent removed.")
	default:
		return errors.New("usage: tw-sync agent install|uninstall")
	}
	return nil
}

func cmdDoctor() error {
	problems := 0
	check := func(ok bool, label, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			problems++
		}
		fmt.Printf("%s %s", mark, label)
		if detail != "" {
			fmt.Printf(": %s", detail)
		}
		fmt.Println()
	}

	task, err := taskBinary()
	check(err == nil, "task installed", task)
	if err != nil {
		return errors.New("doctor found problems")
	}
	version, _ := exec.Command(task, "--version").Output()
	v := strings.TrimSpace(string(version))
	check(strings.HasPrefix(v, "3."), "task version 3.x", v)

	get := func(name string) string {
		out, _ := exec.Command(task, "_get", "rc."+name).Output()
		return strings.TrimSpace(string(out))
	}
	check(get("sync.aws.bucket") != "", "sync.aws.bucket set", get("sync.aws.bucket"))
	check(get("sync.aws.default_credentials") != "", "default credentials enabled", "")
	for _, name := range []string{"sync.aws.profile", "sync.aws.access_key_id",
		"sync.aws.secret_access_key", "sync.local.server_dir", "sync.server.url"} {
		check(get(name) == "", name+" unset", "")
	}
	for _, k := range secretKeys {
		_, err := keychainGet(k)
		check(err == nil, "keychain item "+k, "")
	}

	s := loadState()
	if s.LastSuccess.IsZero() {
		check(false, "last successful sync", "never")
	} else {
		age := time.Since(s.LastSuccess).Round(time.Minute)
		check(age < staleAfter, "last successful sync", age.String()+" ago")
	}
	check(s.Failures == 0, "recent failures", strconv.Itoa(s.Failures))

	_, statErr := os.Stat(plistPath())
	check(statErr == nil, "agent plist installed", "")
	err = exec.Command("/bin/launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), agentLabel)).Run()
	check(err == nil, "agent loaded", "")

	if problems > 0 {
		return fmt.Errorf("doctor found %d problem(s)", problems)
	}
	return nil
}
