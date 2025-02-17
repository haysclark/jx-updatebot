package pr

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/jenkins-x-plugins/jx-gitops/pkg/cmd/git/setup"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient"
	"github.com/jenkins-x/jx-helpers/v3/pkg/helmer"
	"github.com/jenkins-x/jx-helpers/v3/pkg/scmhelpers"
	"github.com/jenkins-x/jx-helpers/v3/pkg/stringhelpers"
	"github.com/shurcooL/githubv4"

	"github.com/jenkins-x-plugins/jx-promote/pkg/environments"
	"github.com/jenkins-x-plugins/jx-updatebot/pkg/apis/updatebot/v1alpha1"
	"github.com/jenkins-x-plugins/jx-updatebot/pkg/rootcmd"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/gitclient/gitdiscovery"
	"github.com/jenkins-x/jx-helpers/v3/pkg/options"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Create a Pull Request on each downstream repository
`)

	cmdExample = templates.Examples(`
		%s pr --test-url https://github.com/myorg/mytest.git
	`)
)

// Options the options for the command
type Options struct {
	environments.EnvironmentPullRequestOptions

	Dir                string
	ConfigFile         string
	Version            string
	VersionFile        string
	PullRequestTitle   string
	PullRequestBody    string
	GitCommitUsername  string
	GitCommitUserEmail string
	AutoMerge          bool
	NoVersion          bool
	GitCredentials     bool
	Labels             []string
	TemplateData       map[string]interface{}
	PullRequestSHAs    map[string]string
	Helmer             helmer.Helmer
	GraphQLClient      *githubv4.Client
	UpdateConfig       v1alpha1.UpdateConfig
}

// NewCmdPullRequest creates a command object for the command
func NewCmdPullRequest() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "pr",
		Short:   "Create a Pull Request on each downstream repository",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the directory look for the VERSION file")
	cmd.Flags().StringVarP(&o.ConfigFile, "config-file", "c", "", "the updatebot config file. If none specified defaults to .jx/updatebot.yaml")
	cmd.Flags().StringVarP(&o.Version, "version", "", "", "the version number to promote. If not specified uses $VERSION or the version file")
	cmd.Flags().StringVarP(&o.VersionFile, "version-file", "", "", "the file to load the version from if not specified directly or via a $VERSION environment variable. Defaults to VERSION in the current dir")
	cmd.Flags().StringVar(&o.PullRequestTitle, "pull-request-title", "", "the PR title")
	cmd.Flags().StringVar(&o.PullRequestBody, "pull-request-body", "", "the PR body")
	cmd.Flags().StringVarP(&o.GitCommitUsername, "git-user-name", "", "", "the user name to git commit")
	cmd.Flags().StringVarP(&o.GitCommitUserEmail, "git-user-email", "", "", "the user email to git commit")
	cmd.Flags().StringSliceVar(&o.Labels, "labels", []string{}, "a list of labels to apply to the PR")
	cmd.Flags().BoolVarP(&o.AutoMerge, "auto-merge", "", true, "should we automatically merge if the PR pipeline is green")
	cmd.Flags().BoolVarP(&o.NoVersion, "no-version", "", false, "disables validation on requiring a '--version' option or environment variable to be required")
	cmd.Flags().BoolVarP(&o.GitCredentials, "git-credentials", "", false, "ensures the git credentials are setup so we can push to git")
	o.EnvironmentPullRequestOptions.ScmClientFactory.AddFlags(cmd)

	eo := &o.EnvironmentPullRequestOptions
	cmd.Flags().StringVarP(&eo.CommitTitle, "commit-title", "", "", "the commit title")
	cmd.Flags().StringVarP(&eo.CommitMessage, "commit-message", "", "", "the commit message")

	return cmd, o
}

// Run implements the command
func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate")
	}

	if o.PullRequestBody == "" || o.CommitMessage == "" {
		// lets try discover the current git URL
		gitURL, err := gitdiscovery.FindGitURLFromDir(o.Dir, true)
		if err != nil {
			log.Logger().Warnf("failed to find git URL %s", err.Error())

		} else if gitURL != "" {
			message := fmt.Sprintf("from: %s\n", gitURL)
			if o.PullRequestBody == "" {
				o.PullRequestBody = message
			}
			if o.CommitMessage == "" {
				o.CommitMessage = message
			}
		}
	}

	for i := range o.UpdateConfig.Spec.Rules {
		rule := &o.UpdateConfig.Spec.Rules[i]
		err = o.FindURLs(rule)
		if err != nil {
			return errors.Wrapf(err, "failed to find URLs")
		}

		o.Fork = rule.Fork
		if len(rule.URLs) == 0 {
			log.Logger().Warnf("no URLs to process for rule %d", i)
		}
		for _, gitURL := range rule.URLs {
			if gitURL == "" {
				log.Logger().Warnf("missing out repository %d as it has no git URL", i)
				continue
			}

			// lets clear the branch name so we create a new one each time in a loop
			o.BranchName = ""

			source := ""
			details := &scm.PullRequest{
				Source: source,
				Title:  o.PullRequestTitle,
				Body:   o.PullRequestBody,
				Draft:  false,
			}

			for _, label := range o.Labels {
				details.Labels = append(details.Labels, &scm.Label{
					Name:        label,
					Description: label,
				})
			}

			o.Function = func() error {
				dir := o.OutDir

				for _, ch := range rule.Changes {
					err := o.ApplyChanges(dir, gitURL, ch)
					if err != nil {
						return errors.Wrapf(err, "failed to apply change")
					}

				}
				if o.PullRequestTitle == "" {
					gitURLpart := strings.Split(gitURL, "/")
					repository := gitURLpart[len(gitURLpart)-2] + "/" + gitURLpart[len(gitURLpart)-1]
					o.PullRequestTitle = fmt.Sprintf("chore(deps): upgrade %s to version %s", repository, o.Version)
				}
				if o.CommitTitle == "" {
					o.CommitTitle = o.PullRequestTitle
				}
				return nil
			}

			// reuse existing PullRequest
			if o.AutoMerge {
				if o.PullRequestFilter == nil {
					o.PullRequestFilter = &environments.PullRequestFilter{}
				}
				if stringhelpers.StringArrayIndex(o.PullRequestFilter.Labels, environments.LabelUpdatebot) < 0 {
					o.PullRequestFilter.Labels = append(o.PullRequestFilter.Labels, environments.LabelUpdatebot)
				}
			}

			pr, err := o.EnvironmentPullRequestOptions.Create(gitURL, "", details, o.AutoMerge)
			if err != nil {
				return errors.Wrapf(err, "failed to create Pull Request on repository %s", gitURL)
			}
			if pr == nil {
				log.Logger().Infof("no Pull Request created")
				continue
			}
			o.AddPullRequest(pr)
		}
	}
	return nil
}

func (o *Options) Validate() error {
	if o.TemplateData == nil {
		o.TemplateData = map[string]interface{}{}
	}
	if o.PullRequestSHAs == nil {
		o.PullRequestSHAs = map[string]string{}
	}
	if o.Version == "" {
		if o.VersionFile == "" {
			o.VersionFile = filepath.Join(o.Dir, "VERSION")
		}
		exists, err := files.FileExists(o.VersionFile)
		if err != nil {
			return errors.Wrapf(err, "failed to check for file %s", o.VersionFile)
		}
		if exists {
			data, err := ioutil.ReadFile(o.VersionFile)
			if err != nil {
				return errors.Wrapf(err, "failed to read version file %s", o.VersionFile)
			}
			o.Version = strings.TrimSpace(string(data))
		} else {
			log.Logger().Infof("version file %s does not exist", o.VersionFile)
		}
	}
	if o.Version == "" {
		o.Version = os.Getenv("VERSION")
		if o.Version == "" && !o.NoVersion {
			return options.MissingOption("version")
		}
	}

	// lets default the config file
	if o.ConfigFile == "" {
		o.ConfigFile = filepath.Join(o.Dir, ".jx", "updatebot.yaml")
	}
	exists, err := files.FileExists(o.ConfigFile)
	if err != nil {
		return errors.Wrapf(err, "failed to check for file %s", o.ConfigFile)
	}
	if exists {
		err = yamls.LoadFile(o.ConfigFile, &o.UpdateConfig)
		if err != nil {
			return errors.Wrapf(err, "failed to load config file %s", o.ConfigFile)
		}
	} else {
		log.Logger().Warnf("file %s does not exist so cannot create any updatebot Pull Requests", o.ConfigFile)
	}

	if o.Helmer == nil {
		o.Helmer = helmer.NewHelmCLIWithRunner(o.CommandRunner, "helm", o.Dir, false)
	}

	// lazy create the git client
	g := o.EnvironmentPullRequestOptions.Git()

	_, _, err = gitclient.EnsureUserAndEmailSetup(g, o.Dir, o.GitCommitUsername, o.GitCommitUserEmail)
	if err != nil {
		return errors.Wrapf(err, "failed to setup git user and email")
	}

	// lets try default the git user/token
	if o.ScmClientFactory.GitToken == "" {
		if o.ScmClientFactory.GitServerURL == "" {
			// lets try discover the git URL
			discover := &scmhelpers.Options{
				Dir:             o.Dir,
				GitClient:       o.Git(),
				CommandRunner:   o.CommandRunner,
				DiscoverFromGit: true,
			}
			err := discover.Validate()
			if err != nil {
				return errors.Wrapf(err, "failed to discover repository details")
			}
			o.ScmClientFactory.GitServerURL = discover.GitServerURL
			o.ScmClientFactory.GitToken = discover.GitToken
		}
		if o.ScmClientFactory.GitServerURL == "" {
			return errors.Errorf("no git-server could be found")
		}
		err = o.ScmClientFactory.FindGitToken()
		if err != nil {
			return errors.Wrapf(err, "failed to find git token")
		}
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = o.ScmClientFactory.GitUsername
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = os.Getenv("GIT_USERNAME")
	}
	if o.GitCommitUsername == "" {
		o.GitCommitUsername = "jenkins-x-bot"
	}

	if o.GitCredentials {
		if o.ScmClientFactory.GitToken == "" {
			return errors.Errorf("missing git token environment variable. Try setting GIT_TOKEN or GITHUB_TOKEN")
		}
		_, gc := setup.NewCmdGitSetup()
		gc.Dir = o.Dir
		gc.DisableInClusterTest = true
		gc.UserEmail = o.GitCommitUserEmail
		gc.UserName = o.GitCommitUsername
		gc.Password = o.ScmClientFactory.GitToken
		gc.GitProviderURL = "https://github.com"
		err = gc.Run()
		if err != nil {
			return errors.Wrapf(err, "failed to setup git credentials file")
		}
		log.Logger().Infof("setup git credentials file for user %s and email %s", gc.UserName, gc.UserEmail)
	}
	return nil
}

// ApplyChanges applies the changes to the given dir
func (o *Options) ApplyChanges(dir, gitURL string, change v1alpha1.Change) error {
	if change.Command != nil {
		return o.ApplyCommand(dir, gitURL, change, change.Command)
	}
	if change.Go != nil {
		return o.ApplyGo(dir, gitURL, change, change.Go)
	}
	if change.Regex != nil {
		return o.ApplyRegex(dir, gitURL, change, change.Regex)
	}
	if change.VersionStream != nil {
		return o.ApplyVersionStream(dir, gitURL, change, change.VersionStream)
	}
	log.Logger().Infof("ignoring unknown change %#v", change)
	return nil
}

func (o *Options) FindURLs(rule *v1alpha1.Rule) error {
	for _, change := range rule.Changes {
		if change.Go != nil {
			err := o.GoFindURLs(rule, change, change.Go)
			if err != nil {
				return errors.Wrapf(err, "failed to find go repositories to update")
			}

		}
	}
	return nil
}
