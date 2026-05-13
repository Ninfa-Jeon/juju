// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package dashboard

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"

	"github.com/juju/cmd/v3"
	"github.com/juju/errors"
	"github.com/juju/gnuflag"
	"github.com/juju/webbrowser"

	"github.com/juju/juju/api/controller/controller"
	jujucmd "github.com/juju/juju/cmd"
	"github.com/juju/juju/cmd/juju/ssh"
	"github.com/juju/juju/cmd/modelcmd"
	"github.com/juju/juju/proxy"
	proxyfactory "github.com/juju/juju/proxy/factory"
)

// ControllerAPI is used to get dashboard info from the controller.
type ControllerAPI interface {
	DashboardConnectionInfo(controller.ProxierFactory) (controller.DashboardConnectionInfo, error)
	Close() error
}

// NewDashboardCommand creates and returns a new dashboard command.
func NewDashboardCommand() cmd.Command {
	d := &dashboardCommand{}
	d.newAPIFunc = func() (ControllerAPI, bool, error) {
		return d.newControllerAPI()
	}
	d.embeddedSSHCmd = ssh.NewSSHCommand(nil, nil, ssh.DefaultSSHRetryStrategy, ssh.DefaultSSHPublicKeyRetryStrategy)
	d.signalCh = make(chan os.Signal)
	return modelcmd.Wrap(d)
}

// dashboardCommand opens the Juju Dashboard in the default browser.
type dashboardCommand struct {
	modelcmd.ModelCommandBase

	hideCreds bool
	browser   bool
	// noController enables bootstrap bridge mode when no controller exists.
	noController bool
	// bridgePort is the port for the localhost bridge server in no-controller mode.
	bridgePort int
	// bridgeBind is the bind address for the bridge server (default "localhost").
	bridgeBind string

	newAPIFunc func() (ControllerAPI, bool, error)

	port           int
	embeddedSSHCmd cmd.Command
	signalCh       chan os.Signal
}

type urlCallBack func(url string)
type connectionRunner func(ctx context.Context, callBack urlCallBack) error

func (c *dashboardCommand) newControllerAPI() (ControllerAPI, bool, error) {
	root, err := c.NewControllerAPIRoot()
	if err != nil {
		return nil, false, errors.Trace(err)
	}
	return controller.NewClient(root), root.IsProxied(), nil
}

const dashboardDoc = `
`

const dashboardExamples = `
Print the Juju Dashboard URL and show admin credential to use to log into it:

	juju dashboard

Print the Juju Dashboard URL only:

	juju dashboard --hide-credential

Open the Juju Dashboard in the default browser and show admin credential to use to log into it:

	juju dashboard --browser

Open the Juju Dashboard in the default browser without printing the login credential:

	juju dashboard --hide-credential --browser

An error is returned if the Juju Dashboard is not running.
`

const dashboardNotAvailableMessage = `The Juju dashboard is not yet deployed.
To deploy the Juju dashboard, follow these steps:
  juju switch controller
  juju deploy juju-dashboard
  juju expose juju-dashboard
  juju relate juju-dashboard controller
`

// Info implements the cmd.Command interface.
func (c *dashboardCommand) Info() *cmd.Info {
	return jujucmd.Info(&cmd.Info{
		Name:     "dashboard",
		Purpose:  "Print the Juju Dashboard URL, or open the Juju Dashboard in the default browser.",
		Doc:      dashboardDoc,
		Examples: dashboardExamples,
	})
}

// SetFlags implements the cmd.Command interface.
func (c *dashboardCommand) SetFlags(f *gnuflag.FlagSet) {
	c.ModelCommandBase.SetFlags(f)
	f.IntVar(&c.port, "port", 8036, "Local port used to serve the dashboard")
	f.BoolVar(&c.hideCreds, "hide-credential", false, "Do not show admin credential to use for logging into the Juju Dashboard")
	f.BoolVar(&c.browser, "browser", false, "Open the web browser, instead of just printing the Juju Dashboard URL")
	f.BoolVar(&c.noController, "no-controller", false, "Start a localhost bridge server for bootstrap when no controller exists")
	f.IntVar(&c.bridgePort, "bridge-port", 17070, "Local port for the bootstrap bridge server (used with --no-controller)")
	f.StringVar(&c.bridgeBind, "bridge-bind", "localhost", "Bind address for the bridge server (use 0.0.0.0 for multipass/remote access)")
}

// SetModelIdentifier overrides the base to skip controller checks in no-controller mode.
// The modelcmd wrapper calls this before Init, so we intercept here.
func (c *dashboardCommand) SetModelIdentifier(modelIdentifier string, allowDefault bool) error {
	if c.noController {
		return nil
	}
	return c.ModelCommandBase.SetModelIdentifier(modelIdentifier, allowDefault)
}

// Init implements the cmd.Command interface.
func (c *dashboardCommand) Init(args []string) error {
	if c.noController {
		return nil
	}
	return c.ModelCommandBase.Init(args)
}

// Run implements the cmd.Command interface.
func (c *dashboardCommand) Run(ctx *cmd.Context) error {
	// Check for no-controller mode first.
	if c.noController {
		return c.runNoControllerMode(ctx)
	}

	api, _, err := c.newAPIFunc()
	if err != nil {
		return errors.Trace(err)
	}
	defer func() { _ = api.Close() }()

	// Check that the Juju Dashboard is available.
	controllerName, err := c.ControllerName()
	if err != nil {
		return errors.Trace(err)
	}

	factory, err := proxyfactory.NewDefaultFactory()
	if err != nil {
		return errors.Annotate(err, "creating default proxy factory to support dashboard connection")
	}

	res, err := api.DashboardConnectionInfo(factory)
	if errors.Is(err, errors.NotFound) {
		return errors.New(dashboardNotAvailableMessage)
	} else if err != nil {
		return errors.Annotatef(err,
			"getting dashboard address for controller %q",
			controllerName,
		)
	}

	var runner connectionRunner

	if res.Proxier != nil {
		tunnelProxy, ok := res.Proxier.(proxy.TunnelProxier)
		if !ok {
			return errors.Annotatef(err, "unsupported proxy type %q for dashboard", res.Proxier.Type())
		}

		runner = tunnelProxyRunner(tunnelProxy)
	} else if res.SSHTunnel != nil {
		runner = tunnelSSHRunner(*res.SSHTunnel, c.port, c.embeddedSSHCmd)
	} else {
		return errors.NotValidf("dashboard connection has no proxying or ssh connection information")
	}

	urlCh := make(chan string)
	defer close(urlCh)
	runnerURLCallBack := func(url string) {
		urlCh <- url
	}

	stdctx, cancel := context.WithCancel(context.Background())
	cancelOnce := sync.Once{}
	defer cancelOnce.Do(cancel)
	finishCh := make(chan error)
	go func() {
		defer close(finishCh)
		err := runner(stdctx, runnerURLCallBack)
		finishCh <- errors.Annotate(err, "running connection runner")
	}()

	// We need to wait for either the runner to blow up or tell us wha the
	// dashboard url is before processing the os signals
	var userErr error
	select {
	case url := <-urlCh:
		if userErr = c.openBrowser(ctx, "Dashboard", url); userErr != nil {
			cancelOnce.Do(cancel)
			break
		}
		if userErr = c.showCredentials(ctx); userErr != nil {
			cancelOnce.Do(cancel)
		}
	case err, ok := <-finishCh:
		if ok {
			return errors.Trace(err)
		}
		return nil
	}

	signal.Notify(c.signalCh, os.Interrupt, os.Kill)
	for {
		select {
		case waitSig := <-c.signalCh:
			ctx.Infof("Received signal %s, stopping dashboard proxy connection", waitSig)
			cancelOnce.Do(cancel)
		case err, ok := <-finishCh:
			if ok && err != nil {
				return errors.Wrap(userErr, err)
			}
			return userErr
		}
	}
}

// runNoControllerMode starts a localhost bridge server for bootstrap when no controller exists.
func (c *dashboardCommand) runNoControllerMode(ctx *cmd.Context) error {
	// Generate a short-lived token for bridge authentication.
	token := generateBridgeToken()

	// Dashboard URL that will be returned after successful bootstrap.
	// For POC, we use a placeholder that would be resolved after bootstrap.
	dashboardURL := fmt.Sprintf("http://localhost:%d", c.port)

	// Create the bridge server with a bootstrap function.
	cfg := BridgeConfig{
		Bind:         c.bridgeBind,
		Port:         c.bridgePort,
		Token:        token,
		BootstrapURL: dashboardURL,
		BootstrapFunc: func(bCtx context.Context, req BootstrapRequest) error {
			return c.runBootstrapFromBridge(bCtx, req)
		},
	}

	bs, err := NewBridgeServer(cfg)
	if err != nil {
		return errors.Annotate(err, "creating bridge server")
	}

	bridgeURL := bs.URL()
	ctx.Infof("Starting bootstrap bridge server at %s", bridgeURL)
	ctx.Infof("Bridge token: %s", token)
	ctx.Infof("Bootstrap UI URL: %s/bootstrap", bridgeURL)

	// Open the bootstrap UI in the browser if requested.
	if c.browser {
		bootstrapUIURL := fmt.Sprintf("%s/bootstrap?token=%s", bridgeURL, token)
		if err := c.openBrowser(ctx, "Bootstrap UI", bootstrapUIURL); err != nil {
			ctx.Infof("Failed to open browser: %v", err)
		}
	}

	// Run the bridge server (blocks until interrupted).
	stdctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- bs.Start(stdctx)
	}()

	signal.Notify(c.signalCh, os.Interrupt, os.Kill)
	select {
	case waitSig := <-c.signalCh:
		ctx.Infof("Received signal %s, stopping bridge server", waitSig)
		cancel()
	case err := <-errCh:
		if err != nil {
			return errors.Annotate(err, "bridge server error")
		}
	}

	return nil
}

// runBootstrapFromBridge invokes the bootstrap path from the bridge server.
//
// POC NOTE: The bootstrap command lives in cmd/juju/commands which imports
// this dashboard package, creating an import cycle. For the POC we use
// exec.Command to invoke the juju binary's bootstrap command directly.
// In a full implementation, the bootstrap logic should be extracted to a
// shared internal package.
func (c *dashboardCommand) runBootstrapFromBridge(ctx context.Context, req BootstrapRequest) error {
	// Build bootstrap command args from the bridge request.
	args := []string{"bootstrap"}
	cloudArg := req.Cloud
	if req.Region != "" {
		cloudArg = req.Cloud + "/" + req.Region
	}
	args = append(args, cloudArg, req.ControllerName)
	if req.CredentialName != "" {
		args = append(args, "--credential", req.CredentialName)
	}

	// Find the juju binary and execute bootstrap.
	// Use PATH lookup (snap-installed juju) so the jujud binary version matches.
	jujuPath, err := exec.LookPath("juju")
	if err != nil {
		return errors.Annotate(err, "finding juju binary")
	}

	cmd := exec.CommandContext(ctx, jujuPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return errors.Annotate(err, "running bootstrap")
	}

	return nil
}

// generateBridgeToken creates a short-lived token for bridge authentication.
func generateBridgeToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "juju-bridge-poc-token-fallback"
	}
	return fmt.Sprintf("juju-bridge-%x", b)
}

func tunnelSSHRunner(
	tunnel controller.DashboardConnectionSSHTunnel,
	localPort int,
	sshCommand cmd.Command,
) connectionRunner {

	args := []string{}
	if tunnel.Entity == "" || tunnel.Model == "" {
		// Backwards compatibility with 3.0.0 controllers that only provide IP address
		args = append(args, "ubuntu@"+tunnel.Host)
	} else {
		args = append(args, "-m", tunnel.Model, tunnel.Entity)
	}
	args = append(args, "-N", "-L",
		fmt.Sprintf("%d:%s", localPort, net.JoinHostPort(tunnel.Host, tunnel.Port)))

	return func(ctx context.Context, callBack urlCallBack) error {
		f := &gnuflag.FlagSet{}
		sshCommand.SetFlags(f)
		err := f.Parse(false, args)
		if err != nil {
			return errors.Trace(err)
		}

		if err := sshCommand.Init(f.Args()); err != nil {
			return errors.Trace(err)
		}

		u := url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort("localhost", strconv.Itoa(localPort)),
		}
		callBack(u.String())

		// TODO(wallyworld) - extract the core ssh machinery and use directly.
		defCtx, err := cmd.DefaultContext()
		if err != nil {
			return errors.Trace(err)
		}
		cmdCtx := defCtx.With(ctx)
		return sshCommand.Run(cmdCtx)
	}
}

func tunnelProxyRunner(p proxy.TunnelProxier) connectionRunner {
	return func(ctx context.Context, callBack urlCallBack) error {
		if err := p.Start(ctx); err != nil {
			return errors.Annotate(err, "starting tunnel proxy")
		}
		defer p.Stop()

		u := url.URL{
			Scheme: "http",
			Host:   net.JoinHostPort(p.Host(), p.Port()),
		}
		callBack(u.String())
		select {
		case <-ctx.Done():
		}
		return nil
	}
}

// openBrowser opens the Juju Dashboard at the given URL.
func (c *dashboardCommand) openBrowser(ctx *cmd.Context, label, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return errors.Annotatef(err, "cannot parse Juju %s URL", label)
	}
	if !c.browser {
		controllerName, err := c.ControllerName()
		if err != nil {
			return errors.Trace(err)
		}
		ctx.Infof("%s for controller %q is enabled at:\n  %s", label, controllerName, u.String())
		return nil
	}
	err = webbrowserOpen(u)
	if err == nil {
		ctx.Infof("Opening the Juju Dashboard in your browser.")
		ctx.Infof("If it does not open, open this URL:\n%s", u)
		return nil
	}
	if err == webbrowser.ErrNoBrowser {
		ctx.Infof("Open this URL in your browser:\n%s", u)
		return nil
	}
	return errors.Annotate(err, "cannot open web browser")
}

// showCredentials shows the admin username and password.
func (c *dashboardCommand) showCredentials(ctx *cmd.Context) error {
	if c.hideCreds {
		return nil
	}
	// TODO(wallyworld) - what to do if we are using a macaroon.
	accountDetails, err := c.CurrentAccountDetails()
	if err != nil {
		return errors.Annotate(err, "cannot retrieve credentials")
	}
	password := accountDetails.Password
	if password == "" {
		// TODO(wallyworld) - fix this
		password = "<unknown> (password has been changed by the user)"
	}
	ctx.Infof("Your login credential is:\n  username: %s\n  password: %s", accountDetails.User, password)
	return nil
}

// webbrowserOpen is defined for testing purposes.
var webbrowserOpen = webbrowser.Open
