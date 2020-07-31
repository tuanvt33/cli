package login

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/AlecAivazis/survey/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/api"
	"github.com/cli/cli/internal/config"
	"github.com/cli/cli/internal/ghinstance"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/cli/cli/pkg/prompt"
	"github.com/cli/cli/utils"
	"github.com/spf13/cobra"
)

// TODO formatting for browser flow text

// TODO clean up and streamline use of api client

// TODO get down to one cfg.Write
// this is tricky; it is now easier to create an httpClient from an in-ram cfg but exposing that
// to the login command is not simple.
// some options:
// 1. a top level helper in api that takes a cfg and returns an http client
// 2. calling api.NewHTTPClient directly and specifying the opts a la httpClient in root.go
// 3. a new factory method (this smells worst; it's just for login which has special needs)
//

type LoginOptions struct {
	HttpClient func() (*http.Client, error)
	IO         *iostreams.IOStreams
	Config     func() (config.Config, error)

	Hostname string
	Token    string
}

func NewCmdLogin(f *cmdutil.Factory, runF func(*LoginOptions) error) *cobra.Command {
	opts := &LoginOptions{
		HttpClient: f.HttpClient,
		IO:         f.IOStreams,
		Config:     f.Config,
	}

	cmd := &cobra.Command{
		Use:   "login",
		Args:  cobra.ExactArgs(0),
		Short: "Authenticate with a GitHub host",
		Long: heredoc.Doc(`Authenticate with a GitHub host.

			This interactive command initializes your authentication state either by helping you log into
			GitHub via browser-based OAuth or by accepting a Personal Access Token.

			The interactivity can be avoided by specifying --with-token and passing a token on STDIN.
		`),
		Example: heredoc.Doc(`
			$ gh auth login
			# => do an interactive setup

			$ gh auth login --with-token < mytoken.txt
			# => read token from mytoken.txt and authenticate against github.com

			$ gh auth login --hostname enterprise.internal --with-token < mytoken.txt
			# => read token from mytoken.txt and authenticate against a GitHub Enterprise instance
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			isTTY := opts.IO.IsStdinTTY()

			if !isTTY && !cmd.Flags().Changed("with-token") {
				return &cmdutil.FlagError{Err: errors.New("--with-token required when not attached to tty")}
			}

			wt, _ := cmd.Flags().GetBool("with-token")
			if wt {
				if isTTY {
					return &cmdutil.FlagError{Err: errors.New("expected token on STDIN")}
				}
				defer opts.IO.In.Close()
				token, err := ioutil.ReadAll(opts.IO.In)
				if err != nil {
					return &cmdutil.FlagError{Err: fmt.Errorf("failed to read token from STDIN: %w", err)}
				}

				opts.Token = strings.TrimSpace(string(token))
				if opts.Hostname == "" {
					opts.Hostname = ghinstance.Default()
				}
			}

			if runF != nil {
				return runF(opts)
			}

			return loginRun(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Hostname, "hostname", "h", "", "The hostname of the GitHub instance to authenticate with")
	cmd.Flags().Bool("with-token", false, "Read token from standard input")

	return cmd
}

func loginRun(opts *LoginOptions) error {
	cfg, err := opts.Config()
	if err != nil {
		return err
	}

	if opts.Token != "" {
		if opts.Hostname == "" {
			return errors.New("empty hostname would leak oauth_token")
		}

		err := validateToken(opts.Hostname, opts.Token)
		if err != nil {
			return err
		}

		return cfg.SetWrite(opts.Hostname, "oauth_token", opts.Token)
	}

	isTTY := opts.IO.IsStdoutTTY() && opts.IO.IsStdinTTY()

	if !isTTY {
		return errors.New("token must be passed via STDIN and --with-token when unattached to TTY")
	}

	// TODO consider explicitly telling survey what io to use since it's implicit right now

	hostname := opts.Hostname

	if hostname == "" {
		var hostType int
		err := prompt.SurveyAskOne(&survey.Select{
			Message: "What account do you want to log into?",
			Options: []string{
				"GitHub.com",
				"GitHub Enterprise",
			},
		}, &hostType)

		if err != nil {
			return fmt.Errorf("could not prompt: %w", err)
		}

		isEnterprise := hostType == 1

		hostname = ghinstance.Default()
		if isEnterprise {
			err := prompt.SurveyAskOne(&survey.Input{
				Message: "GHE hostname:",
			}, &hostname, survey.WithValidator(survey.Required))
			if err != nil {
				return fmt.Errorf("could not prompt: %w", err)
			}
		}
	}

	fmt.Fprintf(opts.IO.ErrOut, "- Logging into %s\n", hostname)

	var authMode int
	err = prompt.SurveyAskOne(&survey.Select{
		Message: "How would you like to authenticate?",
		Options: []string{
			"Login with a web browser",
			"Paste an authentication token",
		},
	}, &authMode)
	if err != nil {
		return fmt.Errorf("could not prompt: %w", err)
	}

	if authMode == 0 {
		_, err := config.AuthFlowWithConfig(cfg, hostname, "")
		if err != nil {
			return fmt.Errorf("failed to authenticate via web browser: %w", err)
		}
	} else {
		fmt.Fprintln(opts.IO.ErrOut)
		fmt.Fprintln(opts.IO.ErrOut, heredoc.Doc(`
				Tip: you can generate a Personal Access Token here https://github.com/settings/tokens
				The minimum required scopes are 'repo' and 'read:org'.`))
		var token string
		err := prompt.SurveyAskOne(&survey.Password{
			Message: "Paste your authentication token:",
		}, &token, survey.WithValidator(survey.Required))
		if err != nil {
			return fmt.Errorf("could not prompt: %w", err)
		}

		if hostname == "" {
			return errors.New("empty hostname would leak oauth_token")
		}

		err = validateToken(hostname, token)
		if err != nil {
			return err
		}

		cfg.SetWrite(opts.Hostname, "oauth_token", opts.Token)
	}

	var gitProtocol string
	err = prompt.SurveyAskOne(&survey.Select{
		Message: "Choose default git protocol",
		Options: []string{
			"HTTPS",
			"SSH",
		},
	}, &gitProtocol)
	if err != nil {
		return fmt.Errorf("could not prompt: %w", err)
	}

	gitProtocol = strings.ToLower(gitProtocol)

	fmt.Fprintf(opts.IO.ErrOut, "- gh config set -h%s git_protocol %s\n", hostname, gitProtocol)
	err = cfg.Set(hostname, "git_protocol", gitProtocol)
	if err != nil {
		return err
	}

	err = cfg.Write()
	if err != nil {
		return err
	}

	greenCheck := utils.Green("✓")
	fmt.Fprintf(opts.IO.ErrOut, "%s Configured git protocol\n", greenCheck)

	httpClient, err := opts.HttpClient()
	if err != nil {
		return err
	}

	apiClient := api.NewClientFromHTTP(httpClient)

	username, err := api.CurrentLoginName(apiClient, hostname)
	if err != nil {
		return fmt.Errorf("error using api: %w", err)
	}

	fmt.Fprintf(opts.IO.ErrOut, "%s Logged in as %s\n", greenCheck, utils.Bold(username))

	return nil
}
