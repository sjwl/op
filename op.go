package op

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"

	"github.com/dvsekhvalnov/jose2go/base64url"
	"github.com/mitchellh/go-homedir"
)

const (
	envPrefix  = "OP_SESSION_"
	configFile = "~/.op/config"
	newLine    = 0xa
)

var authRequired = regexp.MustCompile("(not currently|Authentication)")
var doesNotExist = regexp.MustCompile("(doesn't seem to be an item|no item found|not found)")

type opConfig struct {
	LatestSignIn *string `json:"latest_signin,omitempty"`
	Accounts     []struct {
		ShortHand string `json:"shorthand"`
	} `json:"accounts"`
}

type opDetails struct {
	Fields []struct {
		Name  string `json:"name,omitempty"`
		Value string `json:"value,omitempty"`
	} `json:"fields,omitempty"`
	NotesPlain string `json:"notesPlain,omitempty"`
}

type opItem struct {
	Title   string    `json:"title"`
	Details opDetails `json:"details"`
}

// Op represents an op session object
type Op struct {
	account   string
	envVar    string
	password  string
	procAttr  *syscall.SysProcAttr
	runner    func(name string, args ...string) (cmd *exec.Cmd)
	setEnv    string
	url       string
	secretKey string
	email     string
}

// Opt represents a function that can operate on an Op pointer
type Opt func(o *Op)

type config interface {
	Read() ([]byte, error)
}

// Mockable interface for reading op config
type configer struct{}

func (c configer) Read() ([]byte, error) {
	var empty []byte
	path, err := homedir.Expand(configFile)
	if err != nil {
		return empty, fmt.Errorf("unable to expand '%s': %v", configFile, err)
	}
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return empty, fmt.Errorf("the op config file %s does not exist. Please sign-in first.", configFile)
	}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return empty, err
	}
	return data, nil
}

// declare the reader implementation here so we can override in testing
var configImpl config = configer{}

// getEnv return an OP_SESSION variable either set in the environment
// or via an explicit sign-in.
func (o *Op) getEnv() error {
	envval := os.Getenv(o.envVar)
	if envval != "" {
		o.setEnv = fmt.Sprintf("%s=%s", o.envVar, envval)
		return nil
	}

	var cmd *exec.Cmd
	// if we have url, email and secretKey defined then login without dependency on ~/.op/config existing
	//   this is useful if running from within a container
	if o.email != "" && o.secretKey != "" && o.url != "" {
		cmd = o.runner("op", "signin", o.url, o.email, o.secretKey)

	} else {
		cmd = o.runner("op", "signin", o.account)
		cmd.SysProcAttr = o.procAttr
	}
	if o.password != "" {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("unable to open stdin pipe for op: %v", err)
		}
		go func() {
			defer stdin.Close()
			io.WriteString(stdin, string(o.password))
		}()
	} else {
		cmd.Stdin = os.Stdin
	}

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("unable to sign-in to %s: %v", o.account, err)
	}
	lookFor := fmt.Sprintf(`export %s="(.*)"`, o.envVar)
	re := regexp.MustCompile(lookFor)
	var session string
	for _, line := range strings.Split(string(out), "\n") {
		output := re.FindStringSubmatch(line)
		if len(output) == 2 {
			session = output[1]
			break
		}
	}
	if session == "" {
		return fmt.Errorf("couldn't find %s in op output", o.envVar)
	}
	o.setEnv = fmt.Sprintf("%s=%s", o.envVar, session)
	return nil
}

func (o *Op) runOp(commands ...string) ([]byte, error) {
	cmdEnv := os.Environ()
	cmdEnv = append(cmdEnv, o.setEnv)
	cmd := o.runner("op", commands...)
	cmd.SysProcAttr = o.procAttr
	// append instead of replacing here as testing can set
	// an env var before we get here
	cmd.Env = append(cmd.Env, cmdEnv...)
	cmdOut, err := cmd.CombinedOutput()
	if err != nil {
		if authRequired.FindString(string(cmdOut)) != "" {
			return []byte{}, fmt.Errorf("found stale %s variable in environment", o.envVar)
		}
		return cmdOut, fmt.Errorf("error running %s: %s", commands, cmdOut)
	}
	if len(cmdOut) > 0 {
		last := len(cmdOut) - 1
		if cmdOut[last] == newLine {
			cmdOut = cmdOut[:last]
		}
	}
	return cmdOut, nil
}

func (o *Op) get(itemType, item string) (oi opItem, err error) {
	out, err := o.runOp("get", itemType, item)
	if err != nil {
		return oi, err
	}
	var i opItem
	err = json.Unmarshal(out, &i)
	if err != nil {
		return oi, fmt.Errorf("unable to unmarshal item data: %v", err)
	}
	return i, nil
}

func (o *Op) delete(itemType, item string) error {
	if cmdOut, err := o.runOp("delete", itemType, item); err != nil {
		if doesNotExist.FindString(string(cmdOut)) != "" {
			return nil
		}
		return err
	}
	return nil
}

func (o *Op) set(itemType, item, category string, detail opDetails) error {

	// Marshal oi into string then encode
	encoded, err := encode(detail)
	if err != nil {
		return err
	}

	if _, err := o.runOp("create", itemType, category, encoded, "--title", item); err != nil {
		return err
	}
	return nil
}

// GetUserPass returns the username and password from an item from the active session
func (o *Op) GetUserPass(item string) (user, pass string, err error) {
	i, err := o.get("item", item)
	if err != nil {
		return "", "", err
	}
	for _, field := range i.Details.Fields {
		switch field.Name {
		case "username":
			user = field.Value
		case "password":
			pass = field.Value
		}
	}
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("couldn't find username and password in '%s'", item)
	}
	return user, pass, nil
}

// GetTotp returns the totp for an item from the active session
func (o *Op) GetTotp(item string) (totp string, err error) {
	out, err := o.runOp("get", "totp", item)
	if err != nil {
		return "", fmt.Errorf("cannot get totp for %s: %v", item, err)
	}
	return string(out), nil
}

// GetSecureNote returns a Secret Note by passing in the item name
func (o *Op) GetSecureNote(item string) (string, error) {
	i, err := o.get("item", item)
	if err != nil {
		return "", err
	}

	return i.Details.NotesPlain, nil
}

// SetSecureNote creates new or replaces existing secure notes
func (o *Op) SetSecureNote(item, note string) error {

	// 1Password doesn't replace existing items automatically
	//  so we will need to delete any existing items first.
	if err := o.delete("item", item); err != nil {
		return err
	}

	detail := opDetails{NotesPlain: note}
	if err := o.set("item", item, "Secure Note", detail); err != nil {
		return err
	}
	return nil
}

// GetUserPass is a top-level function that wraps the underlying method from Op
func GetUserPass(item string) (user, pass string, err error) {
	o, err := New()
	if err != nil {
		return "", "", err
	}
	return o.GetUserPass(item)
}

// GetTotp is a top-level function that wraps the underlying method from Op
func GetTotp(item string) (totp string, err error) {
	o, err := New()
	if err != nil {
		return "", err
	}
	return o.GetTotp(item)
}

// GetUserPassTotp gets all three entries for an item
func GetUserPassTotp(item string) (user, pass, totp string, err error) {
	o, err := New()
	if err != nil {
		return "", "", "", err
	}
	u, p, err := o.GetUserPass(item)
	if err != nil {
		return "", "", "", err
	}
	t, err := o.GetTotp(item)
	if err != nil {
		return "", "", "", err
	}
	return u, p, t, nil
}

// New returns a pointer to a configured Op object
func New(opts ...Opt) (o *Op, err error) {
	o = &Op{runner: runCmd}
	for _, opt := range opts {
		opt(o)
	}
	if o.account == "" {
		o.account, err = getSigninFromConfig()
		if err != nil {
			return o, err
		}
	}
	o.envVar = fmt.Sprintf("%s%s", envPrefix, o.account)
	err = o.getEnv()
	if err != nil {
		return o, err
	}
	return o, nil
}

// WithAccount explicitly sets the account to sign-in to
func WithAccount(name string) Opt {
	return func(o *Op) {
		o.account = name
	}
}

// WithPassword sets the password that will be used to sign-in to op
func WithPassword(password string) Opt {
	return func(o *Op) {
		o.password = password
	}
}

// WithUID sets the uid that will be used when running the op command
// Assumes the caller has privs for SYS_SETUID
func WithUID(uid int) Opt {
	return func(o *Op) {
		o.procAttr = &syscall.SysProcAttr{
			Credential: &syscall.Credential{
				Uid: uint32(uid),
			},
		}
	}
}

// WithSecretKey sets the secret key used for op signin
func WithSecretKey(secretKey string) Opt {
	return func(o *Op) {
		o.secretKey = secretKey
	}
}

// WithEmail sets the email used for op signin
func WithEmail(email string) Opt {
	return func(o *Op) {
		o.email = email
	}
}

// WithURL sets the url used for op signin
func WithURL(url string) Opt {
	return func(o *Op) {
		o.url = url
	}
}

// allow specification of an alternate Cmdfunc for testing
func withCmdFunc(f func(name string, args ...string) (cmd *exec.Cmd)) Opt {
	return func(o *Op) {
		o.runner = f
	}
}

// runCmd returns a properly initialized exec Cmd struct
func runCmd(name string, args ...string) (cmd *exec.Cmd) {
	cmd = exec.Command(name, args...)
	return cmd
}

func getSigninFromConfig() (string, error) {
	data, err := configImpl.Read()
	if err != nil {
		return "", err
	}
	var c opConfig
	err = json.Unmarshal(data, &c)
	if err != nil {
		return "", fmt.Errorf("unable to unmarshal config data: %v", err)
	}
	if c.LatestSignIn != nil {
		return *c.LatestSignIn, nil
	}
	acctCount := len(c.Accounts)
	if acctCount > 1 {
		return "", fmt.Errorf("found %d accounts - please supply an explicit name", acctCount)
	}
	if acctCount == 1 {
		return c.Accounts[0].ShortHand, nil
	}
	return "", fmt.Errorf("cannot determine which 1password account to use")
}

func encode(data interface{}) (string, error) {
	bytes, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	return base64url.Encode(bytes), nil
}
