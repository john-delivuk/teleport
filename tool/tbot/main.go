/*
Copyright 2021-2022 Gravitational, Inc.

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

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/constants"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/client"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/tool/tbot/config"
	"github.com/gravitational/teleport/tool/tbot/destination"
	"github.com/gravitational/teleport/tool/tbot/identity"
	"github.com/gravitational/trace"
	"github.com/kr/pretty"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

var log = logrus.WithFields(logrus.Fields{
	trace.Component: teleport.ComponentTBot,
})

const (
	authServerEnvVar = "TELEPORT_AUTH_SERVER"
	tokenEnvVar      = "TELEPORT_BOT_TOKEN"
)

func main() {
	if err := Run(os.Args[1:]); err != nil {
		utils.FatalError(err)
		trace.DebugReport(err)
	}
}

func Run(args []string) error {
	var cf config.CLIConf
	utils.InitLogger(utils.LoggingForDaemon, logrus.InfoLevel)

	app := utils.InitCLIParser("tbot", "tbot: Teleport Machine ID").Interspersed(false)
	app.Flag("debug", "Verbose logging to stdout").Short('d').BoolVar(&cf.Debug)
	app.Flag("config", "Path to a configuration file. Defaults to `/etc/tbot.yaml` if unspecified.").Short('c').StringVar(&cf.ConfigPath)

	versionCmd := app.Command("version", "Print the version")

	startCmd := app.Command("start", "Starts the renewal bot, writing certificates to the data dir at a set interval.")
	startCmd.Flag("auth-server", "Address of the Teleport Auth Server (On-Prem installs) or Proxy Server (Cloud installs).").Short('a').Envar(authServerEnvVar).StringVar(&cf.AuthServer)
	startCmd.Flag("token", "A bot join token, if attempting to onboard a new bot; used on first connect.").Envar(tokenEnvVar).StringVar(&cf.Token)
	startCmd.Flag("ca-pin", "CA pin to validate the Teleport Auth Server; used on first connect.").StringsVar(&cf.CAPins)
	startCmd.Flag("data-dir", "Directory to store internal bot data. Access to this directory should be limited.").StringVar(&cf.DataDir)
	startCmd.Flag("destination-dir", "Directory to write short-lived machine certificates.").StringVar(&cf.DestinationDir)
	startCmd.Flag("certificate-ttl", "TTL of short-lived machine certificates.").Default("60m").DurationVar(&cf.CertificateTTL)
	startCmd.Flag("renewal-interval", "Interval at which short-lived certificates are renewed; must be less than the certificate TTL.").DurationVar(&cf.RenewalInterval)
	startCmd.Flag("join-method", "Method to use to join the cluster, can be \"token\" or \"iam\".").Default(config.DefaultJoinMethod).EnumVar(&cf.JoinMethod, "token", "iam")
	startCmd.Flag("oneshot", "If set, quit after the first renewal.").BoolVar(&cf.Oneshot)

	initCmd := app.Command("init", "Initialize a certificate destination directory for writes from a separate bot user.")
	initCmd.Flag("destination-dir", "Directory to write short-lived machine certificates to.").StringVar(&cf.DestinationDir)
	initCmd.Flag("owner", "Defines Linux \"user:group\" owner of \"--destination-dir\". Defaults to the Linux user running tbot if unspecified.").StringVar(&cf.Owner)
	initCmd.Flag("bot-user", "Enables POSIX ACLs and defines Linux user that can read/write short-lived certificates to \"--destination-dir\".").StringVar(&cf.BotUser)
	initCmd.Flag("reader-user", "Enables POSIX ACLs and defines Linux user that will read short-lived certificates from \"--destination-dir\".").StringVar(&cf.ReaderUser)
	initCmd.Flag("init-dir", "If using a config file and multiple destinations are configured, controls which destination dir to configure.").StringVar(&cf.InitDir)
	initCmd.Flag("clean", "If set, remove unexpected files and directories from the destination.").BoolVar(&cf.Clean)

	configCmd := app.Command("config", "Parse and dump a config file").Hidden()

	watchCmd := app.Command("watch", "Watch a destination directory for changes.").Hidden()

	command, err := app.Parse(args)
	if err != nil {
		app.Usage(args)
		return trace.Wrap(err)
	}

	// While in debug mode, send logs to stdout.
	if cf.Debug {
		utils.InitLogger(utils.LoggingForDaemon, logrus.DebugLevel)
	}

	botConfig, err := config.FromCLIConf(&cf)
	if err != nil {
		return trace.Wrap(err)
	}

	switch command {
	case versionCmd.FullCommand():
		err = onVersion()
	case startCmd.FullCommand():
		err = onStart(botConfig)
	case configCmd.FullCommand():
		err = onConfig(botConfig)
	case initCmd.FullCommand():
		err = onInit(botConfig, &cf)
	case watchCmd.FullCommand():
		err = onWatch(botConfig)
	default:
		// This should only happen when there's a missing switch case above.
		err = trace.BadParameter("command %q not configured", command)
	}

	return err
}

func onVersion() error {
	utils.PrintVersion()
	return nil
}

func onConfig(botConfig *config.BotConfig) error {
	pretty.Println(botConfig)

	return nil
}

func onWatch(botConfig *config.BotConfig) error {
	return trace.NotImplemented("watch not yet implemented")
}

func onStart(botConfig *config.BotConfig) error {
	if botConfig.AuthServer == "" {
		return trace.BadParameter("an auth or proxy server must be set via --auth-server or configuration")
	}

	// First, try to make sure all destinations are usable.
	if err := checkDestinations(botConfig); err != nil {
		return trace.Wrap(err)
	}

	// Start by loading the bot's primary destination.
	dest, err := botConfig.Storage.GetDestination()
	if err != nil {
		return trace.Wrap(err, "could not read bot storage destination from config")
	}

	reloadChan := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go handleSignals(reloadChan, cancel)

	configTokenHashBytes := []byte{}
	if botConfig.Onboarding != nil && botConfig.Onboarding.Token != "" {
		sha := sha256.Sum256([]byte(botConfig.Onboarding.Token))
		configTokenHashBytes = []byte(hex.EncodeToString(sha[:]))
	}

	var authClient auth.ClientI

	// First, attempt to load an identity from storage.
	ident, err := identity.LoadIdentity(dest, identity.BotKinds()...)
	if err == nil && !hasTokenChanged(ident.TokenHashBytes, configTokenHashBytes) {
		identStr, err := describeTLSIdentity(ident)
		if err != nil {
			return trace.Wrap(err)
		}

		log.Infof("Successfully loaded bot identity, %s", identStr)

		if err := checkIdentity(ident); err != nil {
			return trace.Wrap(err)
		}

		if botConfig.Onboarding != nil {
			log.Warn("Note: onboarding config ignored as identity was loaded from persistent storage")
		}

		authClient, err = authenticatedUserClientFromIdentity(ctx, ident, botConfig.AuthServer)
		if err != nil {
			return trace.Wrap(err)
		}
	} else {
		// If the identity can't be loaded, assume we're starting fresh and
		// need to generate our initial identity from a token

		if ident != nil {
			// If ident is set here, we detected a token change above.
			log.Warnf("Detected a token change, will attempt to fetch a new identity.")
		}

		// TODO: validate that errors from LoadIdentity are sanely typed; we
		// actually only want to ignore NotFound errors

		// Verify we can write to the destination.
		if err := identity.VerifyWrite(dest); err != nil {
			return trace.Wrap(err, "Could not write to destination %s, aborting.", dest)
		}

		// Get first identity
		ident, err = getIdentityFromToken(botConfig)
		if err != nil {
			return trace.Wrap(err)
		}

		log.Debug("Attempting first connection using initial auth client")
		authClient, err = authenticatedUserClientFromIdentity(ctx, ident, botConfig.AuthServer)
		if err != nil {
			return trace.Wrap(err)
		}

		// Attempt a request to make sure our client works.
		if _, err := authClient.Ping(ctx); err != nil {
			return trace.Wrap(err, "unable to communicate with auth server")
		}

		identStr, err := describeTLSIdentity(ident)
		if err != nil {
			return trace.Wrap(err)
		}
		log.Infof("Successfully generated new bot identity, %s", identStr)

		log.Debugf("Storing new bot identity to %s", dest)
		if err := identity.SaveIdentity(ident, dest, identity.BotKinds()...); err != nil {
			return trace.Wrap(err, "unable to save generated identity back to destination")
		}
	}

	watcher, err := authClient.NewWatcher(ctx, types.Watch{
		Kinds: []types.WatchKind{{
			Kind: types.KindCertAuthority,
		}},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	go watchCARotations(watcher)

	defer watcher.Close()

	return renewLoop(ctx, botConfig, authClient, ident, reloadChan)
}

func hasTokenChanged(configTokenBytes, identityBytes []byte) bool {
	if len(configTokenBytes) == 0 || len(identityBytes) == 0 {
		return false
	}

	return !bytes.Equal(identityBytes, configTokenBytes)
}

// checkDestinations checks all destinations and tries to create any that
// don't already exist.
func checkDestinations(cfg *config.BotConfig) error {
	// Note: This is vaguely problematic as we don't recommend that users
	// store renewable certs under the same user as end-user certs. That said,
	//  - if the destination was properly created via tbot init this is a no-op
	//  - if users intend to follow that advice but miss a step, it should fail
	//    due to lack of permissions
	storage, err := cfg.Storage.GetDestination()
	if err != nil {
		return trace.Wrap(err)
	}

	// TODO: consider warning if ownership of all destintions is not expected.

	if err := storage.Init(); err != nil {
		return trace.Wrap(err)
	}

	for _, dest := range cfg.Destinations {
		destImpl, err := dest.GetDestination()
		if err != nil {
			return trace.Wrap(err)
		}

		if err := destImpl.Init(); err != nil {
			return trace.Wrap(err)
		}
	}

	return nil
}

// checkIdentity performs basic startup checks on an identity and loudly warns
// end users if it is unlikely to work.
func checkIdentity(ident *identity.Identity) error {
	var validAfter time.Time
	var validBefore time.Time

	if ident.X509Cert != nil {
		validAfter = ident.X509Cert.NotBefore
		validBefore = ident.X509Cert.NotAfter
	} else if ident.SSHCert != nil {
		validAfter = time.Unix(int64(ident.SSHCert.ValidAfter), 0)
		validBefore = time.Unix(int64(ident.SSHCert.ValidBefore), 0)
	} else {
		return trace.BadParameter("identity is invalid and contains no certificates")
	}

	now := time.Now().UTC()
	if now.After(validBefore) {
		log.Errorf(
			"Identity has expired. The renewal is likely to fail. (expires: %s, current time: %s)",
			validBefore.Format(time.RFC3339),
			now.Format(time.RFC3339),
		)
	} else if now.Before(validAfter) {
		log.Warnf(
			"Identity is not yet valid. Confirm that the system time is correct. (valid after: %s, current time: %s)",
			validAfter.Format(time.RFC3339),
			now.Format(time.RFC3339),
		)
	}

	return nil
}

// handleSignals handles incoming Unix signals.
func handleSignals(reload chan struct{}, cancel context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)

	for signal := range signals {
		switch signal {
		case syscall.SIGINT:
			log.Info("Received interrupt, cancelling...")
			cancel()
			return
		case syscall.SIGHUP, syscall.SIGUSR1:
			log.Info("Received reload signal, reloading...")
			reload <- struct{}{}
		}
	}
}

func watchCARotations(watcher types.Watcher) {
	for {
		select {
		case event := <-watcher.Events():
			log.Debugf("CA event: %+v", event)
			// TODO: handle CA rotations
		case <-watcher.Done():
			if err := watcher.Error(); err != nil {
				log.WithError(err).Warnf("error watching for CA rotations")
			}
			return
		}
	}
}

func getIdentityFromToken(cfg *config.BotConfig) (*identity.Identity, error) {
	if cfg.Onboarding == nil {
		return nil, trace.BadParameter("onboarding config required via CLI or YAML")
	}
	if cfg.Onboarding.Token == "" {
		return nil, trace.BadParameter("unable to start: no token present")
	}
	addr, err := utils.ParseAddr(cfg.AuthServer)
	if err != nil {
		return nil, trace.WrapWithMessage(err, "invalid auth server address %+v", cfg.AuthServer)
	}

	tlsPrivateKey, sshPublicKey, tlsPublicKey, err := generateKeys()
	if err != nil {
		return nil, trace.WrapWithMessage(err, "unable to generate new keypairs")
	}

	log.Info("Attempting to generate new identity from token")
	params := auth.RegisterParams{
		Token: cfg.Onboarding.Token,
		ID: auth.IdentityID{
			Role: types.RoleBot,
		},
		Servers:            []utils.NetAddr{*addr},
		PublicTLSKey:       tlsPublicKey,
		PublicSSHKey:       sshPublicKey,
		CAPins:             cfg.Onboarding.CAPins,
		CAPath:             cfg.Onboarding.CAPath,
		GetHostCredentials: client.HostCredentials,
		JoinMethod:         cfg.Onboarding.JoinMethod,
	}
	certs, err := auth.Register(params)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sha := sha256.Sum256([]byte(params.Token))
	tokenHash := hex.EncodeToString(sha[:])
	ident, err := identity.ReadIdentityFromStore(&identity.LoadIdentityParams{
		PrivateKeyBytes: tlsPrivateKey,
		PublicKeyBytes:  sshPublicKey,
		TokenHashBytes:  []byte(tokenHash),
	}, certs, identity.BotKinds()...)
	return ident, trace.Wrap(err)
}

func renewIdentityViaAuth(
	ctx context.Context,
	client auth.ClientI,
	currentIdentity *identity.Identity,
	cfg *config.BotConfig,
) (*identity.Identity, error) {
	// TODO: enforce expiration > renewal period (by what margin?)

	// If using the IAM join method we always go through the initial join flow
	// and fetch new nonrenewable certs
	var joinMethod types.JoinMethod
	if cfg.Onboarding != nil {
		joinMethod = cfg.Onboarding.JoinMethod
	}
	switch joinMethod {
	case types.JoinMethodIAM:
		ident, err := getIdentityFromToken(cfg)
		return ident, trace.Wrap(err)
	default:
	}

	// Ask the auth server to generate a new set of certs with a new
	// expiration date.
	certs, err := client.GenerateUserCerts(ctx, proto.UserCertsRequest{
		PublicKey: currentIdentity.PublicKeyBytes,
		Username:  currentIdentity.X509Cert.Subject.CommonName,
		Expires:   time.Now().Add(cfg.CertificateTTL),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	newIdentity, err := identity.ReadIdentityFromStore(
		currentIdentity.Params(),
		certs,
		identity.BotKinds()...,
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return newIdentity, nil
}

// fetchDefaultRoles requests the bot's own role from the auth server and
// extracts its full list of allowed roles.
func fetchDefaultRoles(ctx context.Context, roleGetter services.RoleGetter, botRole string) ([]string, error) {
	role, err := roleGetter.GetRole(ctx, botRole)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	conditions := role.GetImpersonateConditions(types.Allow)
	return conditions.Roles, nil
}

// describeTLSIdentity writes an informational message about the given identity to
// the log.
func describeTLSIdentity(ident *identity.Identity) (string, error) {
	cert := ident.X509Cert
	if cert == nil {
		return "", trace.BadParameter("attempted to describe TLS identity without TLS credentials")
	}

	tlsIdent, err := tlsca.FromSubject(cert.Subject, cert.NotAfter)
	if err != nil {
		return "", trace.Wrap(err, "bot TLS certificate can not be parsed as an identity")
	}

	var principals []string
	for _, principal := range tlsIdent.Principals {
		if !strings.HasPrefix(principal, constants.NoLoginPrefix) {
			principals = append(principals, principal)
		}
	}

	duration := cert.NotAfter.Sub(cert.NotBefore)
	return fmt.Sprintf(
		"valid: after=%v, before=%v, duration=%s | kind=tls, renewable=%v, disallow-reissue=%v, roles=%v, principals=%v, generation=%v",
		cert.NotBefore.Format(time.RFC3339),
		cert.NotAfter.Format(time.RFC3339),
		duration,
		tlsIdent.Renewable,
		tlsIdent.DisallowReissue,
		tlsIdent.Groups,
		principals,
		tlsIdent.Generation,
	), nil
}

// describeSSHIdentity writes an informational message about the given SSH
// identity to the log.
func describeSSHIdentity(ident *identity.Identity) (string, error) {
	cert := ident.SSHCert
	if cert == nil {
		return "", trace.BadParameter("attempted to describe SSH identity without SSH credentials")
	}

	renewable := false
	if _, ok := cert.Extensions[teleport.CertExtensionRenewable]; ok {
		renewable = true
	}

	disallowReissue := false
	if _, ok := cert.Extensions[teleport.CertExtensionDisallowReissue]; ok {
		disallowReissue = true
	}

	var roles []string
	if rolesStr, ok := cert.Extensions[teleport.CertExtensionTeleportRoles]; ok {
		if actualRoles, err := services.UnmarshalCertRoles(rolesStr); err == nil {
			roles = actualRoles
		}
	}

	var principals []string
	for _, principal := range cert.ValidPrincipals {
		if !strings.HasPrefix(principal, constants.NoLoginPrefix) {
			principals = append(principals, principal)
		}
	}

	duration := time.Second * time.Duration(cert.ValidBefore-cert.ValidAfter)
	return fmt.Sprintf(
		"valid: after=%v, before=%v, duration=%s | kind=ssh, renewable=%v, disallow-reissue=%v, roles=%v, principals=%v",
		time.Unix(int64(cert.ValidAfter), 0).Format(time.RFC3339),
		time.Unix(int64(cert.ValidBefore), 0).Format(time.RFC3339),
		duration,
		renewable,
		disallowReissue,
		roles,
		principals,
	), nil
}

// renew performs a single renewal
func renew(
	ctx context.Context, cfg *config.BotConfig, client auth.ClientI,
	ident *identity.Identity, botDestination destination.Destination,
) (auth.ClientI, *identity.Identity, error) {
	// Make sure we can still write to the bot's destination.
	if err := identity.VerifyWrite(botDestination); err != nil {
		return nil, nil, trace.Wrap(err, "Cannot write to destination %s, aborting.", botDestination)
	}

	log.Debug("Attempting to renew bot certificates...")
	newIdentity, err := renewIdentityViaAuth(ctx, client, ident, cfg)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	identStr, err := describeTLSIdentity(ident)
	if err != nil {
		return nil, nil, trace.Wrap(err, "Could not describe bot identity at %s", botDestination)
	}

	log.Infof("Successfully renewed bot certificates, %s", identStr)

	// TODO: warn if duration < certTTL? would indicate TTL > server's max renewable cert TTL
	// TODO: error if duration < renewalInterval? next renewal attempt will fail

	// Immediately attempt to reconnect using the new identity (still
	// haven't persisted the known-good certs).
	newClient, err := authenticatedUserClientFromIdentity(ctx, newIdentity, cfg.AuthServer)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// Attempt a request to make sure our client works.
	// TODO: consider a retry/backoff loop.
	if _, err := newClient.Ping(ctx); err != nil {
		return nil, nil, trace.Wrap(err, "unable to communicate with auth server")
	}

	log.Debug("Auth client now using renewed credentials.")
	client = newClient
	ident = newIdentity

	// Now that we're sure the new creds work, persist them.
	if err := identity.SaveIdentity(newIdentity, botDestination, identity.BotKinds()...); err != nil {
		return nil, nil, trace.Wrap(err)
	}

	// Determine the default role list based on the bot role. The role's
	// name should match the certificate's Key ID (user and role names
	// should all match bot-$name)
	botResourceName := ident.X509Cert.Subject.CommonName
	defaultRoles, err := fetchDefaultRoles(ctx, client, botResourceName)
	if err != nil {
		log.WithError(err).Warnf("Unable to determine default roles, no roles will be requested if unspecified")
		defaultRoles = []string{}
	}

	// Next, generate impersonated certs
	expires := ident.X509Cert.NotAfter
	for _, dest := range cfg.Destinations {
		destImpl, err := dest.GetDestination()
		if err != nil {
			return nil, nil, trace.Wrap(err)
		}

		// Check the ACLs. We can't fix them, but we can warn if they're
		// misconfigured. We'll need to precompute a list of keys to check.
		// Note: This may only log a warning, depending on configuration.
		if err := destImpl.Verify(identity.ListKeys(dest.Kinds...)); err != nil {
			return nil, nil, trace.Wrap(err)
		}

		// Ensure this destination is also writable. This is a hard fail if
		// ACLs are misconfigured, regardless of configuration.
		// TODO: consider not making these a hard error? e.g. write other
		// destinations even if this one is broken?
		if err := identity.VerifyWrite(destImpl); err != nil {
			return nil, nil, trace.Wrap(err, "Could not write to destination %s, aborting.", destImpl)
		}

		var desiredRoles []string
		if len(dest.Roles) > 0 {
			desiredRoles = dest.Roles
		} else {
			log.Debugf("Destination specified no roles, defaults will be requested: %v", defaultRoles)
			desiredRoles = defaultRoles
		}

		impersonatedIdent, err := generateImpersonatedIdentity(ctx, client, ident, expires, desiredRoles, dest.Kinds)
		if err != nil {
			return nil, nil, trace.Wrap(err, "Failed to generate impersonated certs for %s: %+v", destImpl, err)
		}

		var impersonatedIdentStr string
		if dest.ContainsKind(identity.KindTLS) {
			impersonatedIdentStr, err = describeTLSIdentity(impersonatedIdent)
			if err != nil {
				return nil, nil, trace.Wrap(err, "could not describe impersonated certs for destination %s", destImpl)
			}
		} else {
			// Note: kinds must contain at least 1 of TLS or SSH
			impersonatedIdentStr, err = describeSSHIdentity(impersonatedIdent)
			if err != nil {
				return nil, nil, trace.Wrap(err, "could not describe impersonated certs for destination %s", destImpl)
			}
		}
		log.Infof("Successfully renewed impersonated certificates for %s, %s", destImpl, impersonatedIdentStr)

		if err := identity.SaveIdentity(impersonatedIdent, destImpl, dest.Kinds...); err != nil {
			return nil, nil, trace.Wrap(err, "failed to save impersonated identity to destination %s", destImpl)
		}

		for _, templateConfig := range dest.Configs {
			template, err := templateConfig.GetConfigTemplate()
			if err != nil {
				return nil, nil, trace.Wrap(err)
			}

			if err := template.Render(ctx, client, impersonatedIdent, dest); err != nil {
				log.WithError(err).Warnf("Failed to render config template %+v", templateConfig)
			}
		}
	}

	log.Infof("Persisted new certificates to disk. Next renewal in approximately %s", cfg.RenewalInterval)
	return newClient, newIdentity, nil
}

func renewLoop(ctx context.Context, cfg *config.BotConfig, client auth.ClientI, ident *identity.Identity, reloadChan chan struct{}) error {
	// TODO: failures here should probably not just end the renewal loop, there
	// should be some retry / back-off logic.

	// TODO: what should this interval be? should it be user configurable?
	// Also, must be < the validity period.
	// TODO: validate that cert is actually renewable.

	log.Infof("Beginning renewal loop: ttl=%s interval=%s", cfg.CertificateTTL, cfg.RenewalInterval)
	if cfg.RenewalInterval > cfg.CertificateTTL {
		log.Errorf(
			"Certificate TTL (%s) is shorter than the renewal interval (%s). The next renewal is likely to fail.",
			cfg.CertificateTTL,
			cfg.RenewalInterval,
		)
	}

	// Determine where the bot should write its internal data (renewable cert
	// etc)
	botDestination, err := cfg.Storage.GetDestination()
	if err != nil {
		return trace.Wrap(err)
	}

	ticker := time.NewTicker(cfg.RenewalInterval)
	defer ticker.Stop()
	for {
		newClient, newIdentity, err := renew(ctx, cfg, client, ident, botDestination)
		if err != nil {
			return trace.Wrap(err)
		}

		if cfg.Oneshot {
			log.Info("Oneshot mode enabled, exiting successfully.")
			break
		}

		client = newClient
		ident = newIdentity

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			continue
		case <-reloadChan:
			continue
		}
	}

	return nil
}

// authenticatedUserClientFromIdentity creates a new auth client from the given
// identity. Note that depending on the connection address given, this may
// attempt to connect via the proxy and therefore requires both SSH and TLS
// credentials.
func authenticatedUserClientFromIdentity(ctx context.Context, id *identity.Identity, authServer string) (auth.ClientI, error) {
	if id.SSHCert == nil || id.X509Cert == nil {
		return nil, trace.BadParameter("auth client requires a fully formed identity")
	}

	tlsConfig, err := id.TLSConfig(nil /* cipherSuites */)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sshConfig, err := id.SSHClientConfig()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	authAddr, err := utils.ParseAddr(authServer)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	authClientConfig := &authclient.Config{
		TLS:         tlsConfig,
		SSH:         sshConfig,
		AuthServers: []utils.NetAddr{*authAddr},
		Log:         log,
	}

	c, err := authclient.Connect(ctx, authClientConfig)
	return c, trace.Wrap(err)
}

func generateImpersonatedIdentity(
	ctx context.Context,
	client auth.ClientI,
	currentIdentity *identity.Identity,
	expires time.Time,
	roleRequests []string,
	kinds []identity.ArtifactKind,
) (*identity.Identity, error) {
	// TODO: enforce expiration > renewal period (by what margin?)

	// Generate a fresh keypair for the impersonated identity. We don't care to
	// reuse keys here: impersonated certs might not be as well-protected so
	// constantly rotating private keys
	privateKey, publicKey, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// First, ask the auth server to generate a new set of certs with a new
	// expiration date.
	certs, err := client.GenerateUserCerts(ctx, proto.UserCertsRequest{
		PublicKey:    publicKey,
		Username:     currentIdentity.X509Cert.Subject.CommonName,
		Expires:      expires,
		RoleRequests: roleRequests,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// The root CA included with the returned user certs will only contain the
	// Teleport User CA. We'll also need the host CA for future API calls.
	localCA, err := client.GetClusterCACert()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	caCerts, err := tlsca.ParseCertificatePEMs(localCA.TLSCA)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Append the host CAs from the auth server.
	for _, cert := range caCerts {
		pemBytes, err := tlsca.MarshalCertificatePEM(cert)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		certs.TLSCACerts = append(certs.TLSCACerts, pemBytes)
	}

	newIdentity, err := identity.ReadIdentityFromStore(&identity.LoadIdentityParams{
		PrivateKeyBytes: privateKey,
		PublicKeyBytes:  publicKey,
	}, certs, kinds...)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return newIdentity, nil
}

func generateKeys() (private, sshpub, tlspub []byte, err error) {
	privateKey, publicKey, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	sshPrivateKey, err := ssh.ParseRawPrivateKey(privateKey)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	tlsPublicKey, err := tlsca.MarshalPublicKeyFromPrivateKeyPEM(sshPrivateKey)
	if err != nil {
		return nil, nil, nil, trace.Wrap(err)
	}

	return privateKey, publicKey, tlsPublicKey, nil
}
