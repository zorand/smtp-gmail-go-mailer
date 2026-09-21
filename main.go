// Command votereminder sends individual voter-registration reminder emails
// through a personal Gmail account over SMTP, authenticating with a 16-char
// app password (2-Step Verification must be enabled on the account).
//
// Pure standard library -- no external dependencies.
//
// Design goals, in priority order:
//   - Don't trip the account send cap. Free-Gmail SMTP is contested (some
//     accounts wall at ~100/24h, the web/API cap is 500), so -max defaults to a
//     conservative 90, and a resume log (-sent-log) lets a big list span days
//     without double-sending.
//   - Pace sends (-per-minute) so a burst doesn't look like spam.
//   - Fail safe: retry transient (4xx / dropped connection) with backoff and a
//     fresh connection; on a 550 quota rejection or 535 auth failure, STOP
//     rather than hammer an exhausted or misconfigured account.
//
// The app password is never taken as a flag value or env var (both leak: flag
// values to `ps`, env to /proc/<pid>/environ). Supply it one of three ways:
//   - -password-file PATH  (recommended, cross-platform incl. Windows)
//   - piped on stdin       e.g. `pass show gmail-app | votereminder ...`
//   - interactive prompt   echo-off on Unix; echoes on Windows (no /dev/tty)
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	smtpHost = "smtp.gmail.com"
	smtpPort = "587" // STARTTLS
)

// errStop aborts the whole run (quota exhausted or auth failed): continuing
// would only extend a block or waste attempts.
var errStop = errors.New("stop run")

type recipient struct {
	Email string
	Name  string
}

func main() {
	var (
		recipPath = flag.String(
			"recipients",
			"recipients.txt",
			"recipient list: one per line, `email` or `email,Name`; # comments and blanks ignored",
		)
		sentPath  = flag.String("sent-log", "sent.log", "CSV resume log (from,to,timestamp) of sends already made; used to skip duplicates")
		emailBody = flag.String("emailbody", "emailbody.txt", "path to the email body file; supports $name and $sender placeholders")
		subject   = flag.String(
			"subject",
			"Reminder: you were going to register to vote",
			"subject line; supports $name and $sender placeholders",
		)
		fromAddr = flag.String("from", "", "your Gmail address (required): SMTP username and envelope/From address")
		fromName = flag.String("from-name", "", "display name for the From header and $sender in the body")
		passFile = flag.String("password-file", "", "read the app password from this file instead of prompting/stdin")
		seedFile = flag.String(
			"secrethashseed",
			"",
			"file holding the secret seed for voting-link HMAC; required when the template uses $votingsecret/$votinghash. Generated (0600) if missing or empty.",
		)
		perMinute = flag.Float64("per-minute", 20, "max sends per minute")
		maxSends  = flag.Int("max", 90, "max sends this run; conservative for free-Gmail SMTP (contested 100/24h vs 500/24h)")
		retries   = flag.Int("retries", 5, "retry attempts per message for transient errors")
		dryRun    = flag.Bool("dry-run", false, "render and log messages without connecting, sending, or writing the sent log")
	)
	flag.Parse()

	if *fromAddr == "" {
		log.Fatal("-from is required (your Gmail address)")
	}
	if *fromName == "" {
		log.Println("warning: -from-name is empty; $sender will render blank")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recipients, err := loadRecipients(*recipPath)
	if err != nil {
		log.Fatalf("loading recipients: %v", err)
	}
	if len(recipients) == 0 {
		log.Fatalf("no valid recipients in %s", *recipPath)
	}

	sent, err := loadSent(*sentPath)
	if err != nil {
		log.Fatalf("loading sent log: %v", err)
	}

	bodyBytes, err := os.ReadFile(*emailBody)
	if err != nil {
		log.Fatalf("reading email body %q: %v", *emailBody, err)
	}
	bodyText := string(bodyBytes)

	// Load the voting seed only if a voting tag is actually used. Missing flag
	// with a voting tag present is a hard error, before anything is sent.
	var seed []byte
	if usesVotingTag(*subject) || usesVotingTag(bodyText) {
		if *seedFile == "" {
			log.Fatalf(
				"the template uses a voting tag ($votingsecret/$votinghash) but -secrethashseed was not given; pass -secrethashseed FILE",
			)
		}
		seed, err = loadOrCreateSeed(*seedFile)
		if err != nil {
			log.Fatalf("loading voting seed: %v", err)
		}
	}

	var m *mailer
	if !*dryRun {
		password, err := loadPassword(*passFile)
		if err != nil {
			log.Fatalf("loading app password: %v", err)
		}
		m = newMailer(*fromAddr, password)
		defer m.close()
	}

	interval := time.Duration(float64(time.Minute) / *perMinute)
	var last time.Time

	var sentCount, skipCount, failCount, consecFails int
	for _, r := range recipients {
		key := strings.ToLower(r.Email)
		if sent[key] {
			skipCount++
			continue
		}
		if sentCount >= *maxSends {
			log.Printf("reached per-run cap (-max=%d); stopping. Re-run later to continue.", *maxSends)
			break
		}

		raw, err := buildMessage(*fromAddr, *fromName, r, *subject, bodyText, seed)
		if err != nil {
			log.Printf("skip %s: building message: %v", r.Email, err)
			failCount++
			continue
		}

		if *dryRun {
			log.Printf("[dry-run] to %s (%s):\n%s", r.Email, r.Name, string(raw))
			sentCount++
			continue
		}

		// Pace: wait out the remainder of the interval since the last send.
		if !last.IsZero() {
			if d := interval - time.Since(last); d > 0 {
				select {
				case <-time.After(d):
				case <-ctx.Done():
					log.Printf("stopping: %v", ctx.Err())
					printSummary(sentCount, skipCount, failCount)
					return
				}
			}
		}
		last = time.Now()

		err = sendWithRetry(ctx, m, *fromAddr, r.Email, raw, *retries)
		switch {
		case err == nil:
			if aerr := appendSent(*sentPath, *fromAddr, r.Email); aerr != nil {
				log.Fatalf(
					"sent to %s but FAILED to record it in %s: %v -- stopping to avoid duplicate sends. Add %s to the log manually before re-running.",
					r.Email,
					*sentPath,
					aerr,
					r.Email,
				)
			}
			sentCount++
			consecFails = 0
			log.Printf("sent to %s (%d this run)", r.Email, sentCount)

		case errors.Is(err, errStop):
			log.Printf("stopping at %s: %v", r.Email, err)
			log.Printf(
				"If this was a quota block, wait ~24h and re-run; it resumes from the sent log. If auth (535), check the app password and that 2-Step Verification is on.",
			)
			printSummary(sentCount, skipCount, failCount)
			return

		default:
			failCount++
			consecFails++
			log.Printf("failed to send to %s: %v", r.Email, err)
		}

		// Stop the run when sends fail back-to-back (network down, a wider
		// block) instead of churning through the rest of the list.
		if consecFails >= 5 {
			log.Printf("5 consecutive failures; stopping in case something is wrong (network or a wider block).")
			break
		}
	}

	printSummary(sentCount, skipCount, failCount)
}

func printSummary(sent, skipped, failed int) {
	log.Printf("done: %d sent, %d skipped (already sent), %d failed", sent, skipped, failed)
}

// ---- SMTP mailer -----------------------------------------------------------

type mailer struct {
	username string
	auth     smtp.Auth
	heloName string
	client   *smtp.Client
}

func newMailer(username, password string) *mailer {
	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "localhost"
	}
	return &mailer{
		username: username,
		auth:     smtp.PlainAuth("", username, password, smtpHost),
		heloName: name,
	}
}

func (m *mailer) connect() error {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(smtpHost, smtpPort), 30*time.Second)
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, smtpHost)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := c.Hello(m.heloName); err != nil {
		_ = c.Close()
		return err
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		_ = c.Close()
		return errors.New("server does not advertise STARTTLS")
	}
	if err := c.StartTLS(&tls.Config{ServerName: smtpHost}); err != nil {
		_ = c.Close()
		return err
	}
	if err := c.Auth(m.auth); err != nil {
		_ = c.Close()
		return err // typically 535 on a bad app password
	}
	m.client = c
	return nil
}

func (m *mailer) close() {
	if m.client != nil {
		_ = m.client.Quit()
		m.client = nil
	}
}

// send performs one SMTP transaction, connecting lazily. On a protocol-level
// rejection (*textproto.Error) the connection is reset and kept; on an I/O
// error it is dropped so the next attempt reconnects.
func (m *mailer) send(from, to string, raw []byte) error {
	if m.client == nil {
		if err := m.connect(); err != nil {
			m.client = nil
			return err
		}
	}
	if err := m.transact(from, to, raw); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) {
			_ = m.client.Reset() // command rejected; connection still usable
		} else {
			m.close() // I/O failure; force a reconnect next time
		}
		return err
	}
	_ = m.client.Reset()
	return nil
}

func (m *mailer) transact(from, to string, raw []byte) error {
	if err := m.client.Mail(from); err != nil {
		return err
	}
	if err := m.client.Rcpt(to); err != nil {
		return err
	}
	w, err := m.client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close() // writes the "." terminator; server's final reply lands here
}

func sendWithRetry(ctx context.Context, m *mailer, from, to string, raw []byte, retries int) error {
	const baseDelay = 2 * time.Second
	const maxDelay = 60 * time.Second

	for attempt := 0; ; attempt++ {
		err := m.send(from, to, raw)
		if err == nil {
			return nil
		}

		switch classify(err) {
		case errClassStop:
			return fmt.Errorf("%w: %v", errStop, err)
		case errClassPermanent:
			return err // bad address / rejected message; skip this recipient
		}

		if attempt >= retries {
			// Persistent transient failure often means the account, not just a
			// momentary hiccup, is throttled. Stop instead of thrashing.
			return fmt.Errorf("%w: giving up after %d attempts: %v", errStop, retries+1, err)
		}
		delay := baseDelay << attempt
		if delay > maxDelay {
			delay = maxDelay
		}
		delay += time.Duration(rand.Int63n(int64(time.Second)))
		log.Printf("transient error (attempt %d/%d), backing off %s: %v", attempt+1, retries, delay.Round(time.Second), err)

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

type errClass int

const (
	errClassTransient errClass = iota
	errClassPermanent
	errClassStop
)

func classify(err error) errClass {
	var te *textproto.Error
	if !errors.As(err, &te) {
		// I/O error, EOF, dial failure -- retry on a fresh connection.
		return errClassTransient
	}

	msg := strings.ToLower(te.Msg)
	switch {
	case te.Code == 535: // authentication failed -- app password/2FA problem
		return errClassStop
	case te.Code == 550 && (strings.Contains(msg, "5.4.5") || strings.Contains(msg, "quota") || strings.Contains(msg, "limit")):
		return errClassStop // daily sending quota exceeded
	case te.Code == 421, te.Code == 454: // service unavailable / temp auth -- back off
		return errClassTransient
	case te.Code >= 400 && te.Code < 500:
		return errClassTransient
	default: // other 5xx: bad recipient, message rejected, etc.
		return errClassPermanent
	}
}

// ---- credentials -----------------------------------------------------------

func loadPassword(passFile string) (string, error) {
	var raw string
	switch {
	case passFile != "":
		b, err := os.ReadFile(passFile)
		if err != nil {
			return "", err
		}
		warnIfWorldReadable(passFile)
		warnIfInGitRepo(passFile)
		raw = string(b)
	case isTerminal(os.Stdin):
		var err error
		if raw, err = promptPassword(); err != nil {
			return "", err
		}
	default: // stdin is a pipe/redirect
		var err error
		if raw, err = readLine(os.Stdin); err != nil {
			return "", err
		}
	}
	pw := normalizePassword(raw)
	if pw == "" {
		return "", errors.New("empty app password")
	}
	return pw, nil
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// warnIfWorldReadable flags a password file that others can read. Unix only:
// Windows FileMode doesn't carry POSIX permission bits, so the check is skipped.
func warnIfWorldReadable(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		log.Printf("warning: %s is readable by group/other (mode %o); consider: chmod 600 %s", path, perm, path)
	}
}

// warnIfInGitRepo flags a password file living inside a git working tree, where
// it's one `git add .` away from being committed. Keep secrets outside the repo.
func warnIfInGitRepo(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	for dir := filepath.Dir(abs); ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			log.Printf(
				"warning: %s is inside a git repository (%s); move the app password outside your repo so it can't be committed",
				path,
				dir,
			)
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir { // reached the filesystem root
			return
		}
		dir = parent
	}
}

// loadOrCreateSeed returns the HMAC key material for voting tokens. If the file
// exists and is non-empty, the key is its contents with surrounding whitespace
// trimmed (so a trailing editor newline doesn't matter) — the vote server must
// derive the key the same way. If the file is missing or empty, a fresh 32-byte
// random seed is generated, base64-encoded, written 0600, and used; this is
// announced loudly because a new seed invalidates every previously issued link.
func loadOrCreateSeed(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if key := strings.TrimSpace(string(b)); key != "" {
			warnIfWorldReadable(path)
			warnIfInGitRepo(path)
			return []byte(key), nil
		}
		// exists but empty -> generate below
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	raw := make([]byte, 32)
	if _, err := crand.Read(raw); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing new voting seed to %s: %w", path, err)
	}
	log.Printf(
		"WARNING: generated a NEW voting seed in %s — links issued under any previous seed will NO LONGER verify. Back this file up and reuse it on every run and on your vote server.",
		path,
	)
	warnIfWorldReadable(path)
	warnIfInGitRepo(path)
	return []byte(key), nil
}

// promptPassword reads a line from the controlling terminal with echo disabled.
// It uses /dev/tty (not stdin) so it works even when stdin is redirected, and
// toggles echo via stty (portable across macOS/Linux; no external Go deps).
func promptPassword() (string, error) {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return readLine(os.Stdin) // no controlling tty; fall back (will echo)
	}
	defer func() { _ = tty.Close() }()

	fmt.Fprint(os.Stderr, "Gmail app password: ")
	restore := setEcho(tty, false)
	line, rerr := readLine(tty)
	restore() // re-enable echo even if the read failed
	fmt.Fprintln(os.Stderr)
	return line, rerr
}

// setEcho toggles terminal echo and returns a function that restores it.
func setEcho(tty *os.File, on bool) (restore func()) {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = tty
	if err := cmd.Run(); err != nil {
		return func() {} // stty unavailable; nothing to restore
	}
	return func() { _ = setEchoOnce(tty, true) }
}

func setEchoOnce(tty *os.File, on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.Command("stty", arg)
	cmd.Stdin = tty
	return cmd.Run()
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return line, nil
}

// normalizePassword strips whitespace: Google displays app passwords as four
// space-separated groups, but SMTP wants the 16 characters unbroken.
func normalizePassword(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// ---- recipients & sent log -------------------------------------------------

func loadRecipients(path string) ([]recipient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var out []recipient
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var email, name string
		if idx := strings.Index(line, ","); idx >= 0 {
			email = strings.TrimSpace(line[:idx])
			name = strings.TrimSpace(line[idx+1:])
		} else {
			email = line
		}
		addr, err := mail.ParseAddress(email)
		if err != nil {
			log.Printf("recipients line %d: skipping invalid address %q: %v", i+1, email, err)
			continue
		}
		key := strings.ToLower(addr.Address)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, recipient{Email: addr.Address, Name: name})
	}
	return out, nil
}

// loadSent builds the set of already-sent recipients from the sent log, keyed
// on the "to" column. It accepts both the CSV format (from,to,timestamp) and
// legacy plain-email lines, and ignores the header and any malformed rows.
func loadSent(path string) (map[string]bool, error) {
	set := make(map[string]bool)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return set, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		to := fields[0] // legacy: bare email
		if len(fields) >= 2 {
			to = fields[1] // CSV: from,to,timestamp
		}
		to = strings.ToLower(strings.TrimSpace(to))
		if strings.Contains(to, "@") { // skips the header row and blanks
			set[to] = true
		}
	}
	return set, nil
}

// appendSent records one send as a CSV row: from,to,timestamp
// (timestamp is local time, YYYYMMDD-HHMMSS). A header is written when the file
// is first created.
func appendSent(path, from, to string) (err error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		// Surface a Close error (e.g. a failed flush) only if the writes
		// themselves succeeded — a dropped record here risks a duplicate send.
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	if fi, err := f.Stat(); err == nil && fi.Size() == 0 {
		if _, err := fmt.Fprintln(f, "from,to,timestamp"); err != nil {
			return err
		}
	}
	ts := time.Now().Format("20060102-150405")
	_, err = fmt.Fprintf(f, "%s,%s,%s\n", strings.ToLower(from), strings.ToLower(to), ts)
	return err
}

// ---- message building ------------------------------------------------------

// expand substitutes the template tags. It uses an exact replacer (not
// os.Expand) so a stray '$' elsewhere in the text is left alone.
func expand(s, name, sender, votingSecret, votingHash string) string {
	return strings.NewReplacer(
		"${votingsecret}", votingSecret, "$votingsecret", votingSecret,
		"${votinghash}", votingHash, "$votinghash", votingHash,
		"${name}", name, "$name", name,
		"${sender}", sender, "$sender", sender,
	).Replace(s)
}

// usesVotingTag reports whether a subject/body references a voting tag (either
// $tag or ${tag} form).
func usesVotingTag(s string) bool {
	return strings.Contains(s, "votingsecret") || strings.Contains(s, "votinghash")
}

// votingToken derives the per-voter authentication token:
//
//	base64url( HMAC-SHA256(seed, LP("vote") ‖ LP(lowerEmail)) )   LP(x)=len(x)":"x
//
// full 32 bytes, URL-safe base64 without padding. The server recomputes this
// with the same seed and constant-time compares (hmac.Equal) to authenticate.
func votingToken(seed []byte, lowerEmail string) string {
	mac := hmac.New(sha256.New, seed)
	writeLP(mac, "vote")
	writeLP(mac, lowerEmail)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// writeLP writes a length-prefixed field: the byte length in ASCII decimal, a
// colon, then the raw bytes. Length-prefixing makes the concatenation of fields
// unambiguous, so no two distinct field sets can produce the same signed bytes.
func writeLP(w io.Writer, s string) {
	// w is always an hmac.Hash here, whose Write is documented never to error.
	_, _ = fmt.Fprintf(w, "%d:", len(s))
	_, _ = io.WriteString(w, s)
}

// buildMessage returns an RFC 5322 message. $name resolves to the recipient's
// name, or their email address when no name was given. When seed is non-nil,
// $votingsecret / $votinghash resolve to a per-voter authentication link.
// Body newlines are plain \n; the SMTP DotWriter normalizes them to CRLF and
// dot-stuffs on the way out.
func buildMessage(fromAddr, fromName string, r recipient, subjectTmpl, bodyTmpl string, seed []byte) ([]byte, error) {
	name := r.Name
	if name == "" {
		name = r.Email
	}

	var votingSecret, votingHash string
	if seed != nil {
		email := strings.ToLower(r.Email)
		votingHash = votingToken(seed, email)
		votingSecret = "voter=" + url.QueryEscape(email) + "&token=" + votingHash
	}

	subject := expand(subjectTmpl, name, fromName, votingSecret, votingHash)
	body := expand(bodyTmpl, name, fromName, votingSecret, votingHash)

	to := r.Email
	if r.Name != "" {
		to = (&mail.Address{Name: r.Name, Address: r.Email}).String()
	}
	from := fromAddr
	if fromName != "" {
		from = (&mail.Address{Name: fromName, Address: fromAddr}).String()
	}

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", from)
	fmt.Fprintf(&msg, "To: %s\r\n", to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&msg, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	msg.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	msg.WriteString("\r\n")
	msg.WriteString(body)

	return []byte(msg.String()), nil
}
