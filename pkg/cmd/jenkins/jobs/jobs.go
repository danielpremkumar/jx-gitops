package jobs

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/sprig"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/jx-gitops/pkg/apis/gitops/v1alpha1"
	"github.com/jenkins-x/jx-gitops/pkg/cmd/jenkins/add"
	"github.com/jenkins-x/jx-gitops/pkg/rootcmd"
	"github.com/jenkins-x/jx-gitops/pkg/sourceconfigs"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/files"
	"github.com/jenkins-x/jx-helpers/v3/pkg/templater"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-helpers/v3/pkg/yamls"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	info = termcolor.ColorInfo

	cmdLong = templates.LongDesc(`
		Generates the Jenkins Jobs helm files
`)

	cmdExample = templates.Examples(`
		# generate the jenkins job files
		%s jenkins jobs

	`)

	jobValuesHeader = `# NOTE this file is autogenerated - DO NOT EDIT!
#
# This file is generated from the template files via the command: 
#    jx gitops jenkins jobs
controller:
  JCasC:
    configScripts:
      jxsetup: |
        jobs:
          - script: |
`
	indent = "              "

	sampleValuesFile = `# custom Jenkins chart configuration
# see https://github.com/jenkinsci/helm-charts/blob/main/charts/jenkins/VALUES_SUMMARY.md

sampleValue: removeMeWhenYouAddRealConfiguration
`
)

// LabelOptions the options for the command
type Options struct {
	Dir                    string
	ConfigFile             string
	OutDir                 string
	DefaultTemplate        string
	NoCreateHelmfile       bool
	SourceConfig           v1alpha1.SourceConfig
	JenkinsServerTemplates map[string][]*JenkinsTemplateConfig
}

// JenkinsTemplateConfig stores the data to render jenkins config files
type JenkinsTemplateConfig struct {
	Server       string
	Key          string
	TemplateFile string
	TemplateText string
	TemplateData map[string]interface{}
}

// NewCmdJenkinsJobs creates a command object for the command
func NewCmdJenkinsJobs() (*cobra.Command, *Options) {
	o := &Options{}

	cmd := &cobra.Command{
		Use:     "jobs",
		Aliases: []string{"job"},
		Short:   "Generates the Jenkins Jobs helm files",
		Long:    cmdLong,
		Example: fmt.Sprintf(cmdExample, rootcmd.BinaryName),
		Run: func(cmd *cobra.Command, args []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&o.Dir, "dir", "d", ".", "the current working directory")
	cmd.Flags().StringVarP(&o.OutDir, "out", "o", "", "the output directory for the generated config files. If not specified defaults to the jenkins dir in the current directory")
	cmd.Flags().StringVarP(&o.ConfigFile, "config", "c", "", "the configuration file to load for the repository configurations. If not specified we look in ./.jx/gitops/source-config.yaml")
	cmd.Flags().StringVarP(&o.DefaultTemplate, "default-template", "", "", "the default job template file if none is configured for a repository")
	cmd.Flags().BoolVarP(&o.NoCreateHelmfile, "no-create-helmfile", "", false, "disables the creation of the helmfiles/jenkinsName/helmfile.yaml file if a jenkins server does not yet exist")
	return cmd, o
}

func (o *Options) Validate() error {
	if o.ConfigFile == "" {
		o.ConfigFile = filepath.Join(o.Dir, ".jx", "gitops", v1alpha1.SourceConfigFileName)
	}
	if o.OutDir == "" {
		o.OutDir = filepath.Join(o.Dir, "helmfiles")
	}

	exists, err := files.FileExists(o.ConfigFile)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", o.ConfigFile)
	}
	if !exists {
		log.Logger().Infof("the source config file %s does not exist", info(o.ConfigFile))
		return nil
	}

	if o.DefaultTemplate != "" {
		exists, err := files.FileExists(o.DefaultTemplate)
		if err != nil {
			return errors.Wrapf(err, "failed to check if file exists %s", o.DefaultTemplate)
		}
		if !exists {
			return errors.Errorf("the default-xml-template file %s does not exist", o.DefaultTemplate)
		}
	}

	err = yamls.LoadFile(o.ConfigFile, &o.SourceConfig)
	if err != nil {
		return errors.Wrapf(err, "failed to load file %s", o.ConfigFile)
	}

	if o.JenkinsServerTemplates == nil {
		o.JenkinsServerTemplates = map[string][]*JenkinsTemplateConfig{}
	}
	return nil
}

func (o *Options) Run() error {
	err := o.Validate()
	if err != nil {
		return errors.Wrapf(err, "failed to validate options")
	}

	config := &o.SourceConfig
	if config.Spec.JenkinsJobTemplate == "" {
		relPath := filepath.Join("jenkins", "templates", "default.job.gotmpl")
		path := filepath.Join(o.Dir, relPath)
		exists, err := files.FileExists(path)
		if err != nil {
			return errors.Wrapf(err, "failed to check if path exists %s", path)
		}
		if exists {
			config.Spec.JenkinsJobTemplate = relPath
		}
	}

	for i := range config.Spec.JenkinsServers {
		server := &config.Spec.JenkinsServers[i]
		for j := range server.Groups {
			group := &server.Groups[j]
			for k := range group.Repositories {
				repo := &group.Repositories[k]
				sourceconfigs.DefaultValues(config, group, repo)
				serverName := server.Server
				jobTemplate := firstNonBlankValue(repo.JenkinsJobTemplate, group.JenkinsJobTemplate, server.JobTemplate, config.Spec.JenkinsJobTemplate)
				err = o.processJenkinsConfig(group, repo, serverName, jobTemplate)
				if err != nil {
					return errors.Wrapf(err, "failed to process Jenkins Config")
				}
			}
		}
	}

	for server, configs := range o.JenkinsServerTemplates {
		dir := filepath.Join(o.OutDir, server)
		err = os.MkdirAll(dir, files.DefaultDirWritePermissions)
		if err != nil {
			return errors.Wrapf(err, "failed to create dir %s", dir)
		}

		err = o.verifyServerHelmfileExists(dir, server)
		if err != nil {
			return errors.Wrapf(err, "failed to verify the jenkins helmfile exists for %s", server)
		}

		path := filepath.Join(dir, "job-values.yaml")
		log.Logger().Infof("creating Jenkins values.yaml file %s", path)

		funcMap := sprig.TxtFuncMap()

		buf := strings.Builder{}
		buf.WriteString(jobValuesHeader)

		for _, jcfg := range configs {
			path := jcfg.TemplateFile
			output, err := templater.Evaluate(funcMap, jcfg.TemplateData, jcfg.TemplateText, path, "Jenkins Server "+server)
			if err != nil {
				return errors.Wrapf(err, "failed to evaluate template %s", path)
			}
			buf.WriteString(indent + "// from template: " + path + "\n")
			buf.WriteString(indentText(output, indent))
			buf.WriteString(indent + "\n")
		}

		err = ioutil.WriteFile(path, []byte(buf.String()), files.DefaultFileWritePermissions)
		if err != nil {
			return errors.Wrapf(err, "failed to save file %s", path)
		}
	}
	return nil
}

func indentText(text string, indent string) string {
	lines := strings.Split(text, "\n")
	return indent + strings.Join(lines, "\n"+indent)
}

func firstNonBlankValue(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (o *Options) processJenkinsConfig(group *v1alpha1.RepositoryGroup, repo *v1alpha1.Repository, server, jobTemplatePath string) error {
	if server == "" {
		log.Logger().Infof("ignoring repository %s as it has no Jenkins server defined", repo.URL)
		return nil
	}
	if jobTemplatePath == "" {
		log.Logger().Infof("ignoring repository %s as it has no Jenkins JobTemplate defined at the repository, group or server level", repo.URL)
		return nil
	}
	jobTemplate := filepath.Join(o.Dir, jobTemplatePath)
	exists, err := files.FileExists(jobTemplate)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", jobTemplate)
	}
	if !exists {
		return errors.Errorf("the jobTemplate file %s does not exist", jobTemplate)
	}

	data, err := ioutil.ReadFile(jobTemplate)
	if err != nil {
		return errors.Wrapf(err, "failed to load file %s", jobTemplate)
	}

	fullName := scm.Join(group.Owner, repo.Name)

	templateData := map[string]interface{}{
		"ID":           group.Owner + "-" + repo.Name,
		"FullName":     fullName,
		"Owner":        group.Owner,
		"GitServerURL": group.Provider,
		"GitKind":      group.ProviderKind,
		"GitName":      group.ProviderName,
		"Repository":   repo.Name,
		"URL":          repo.URL,
		"CloneURL":     repo.HTTPCloneURL,
	}

	o.JenkinsServerTemplates[server] = append(o.JenkinsServerTemplates[server], &JenkinsTemplateConfig{
		Server:       server,
		Key:          fullName,
		TemplateFile: jobTemplate,
		TemplateText: string(data),
		TemplateData: templateData,
	})
	return nil
}

func (o *Options) verifyServerHelmfileExists(dir string, server string) error {
	path := filepath.Join(dir, "helmfile.yaml")
	exists, err := files.FileExists(path)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", path)
	}
	if exists {
		return nil
	}

	_, ao := add.NewCmdJenkinsAdd()
	ao.Name = server
	ao.Dir = o.Dir
	ao.Values = []string{"job-values.yaml", "values.yaml"}
	err = ao.Run()
	if err != nil {
		return errors.Wrapf(err, "failed to add jenkins server")
	}

	// lets check if there's a values.yaml file and if not create one
	path = filepath.Join(dir, "values.yaml")
	exists, err = files.FileExists(path)
	if err != nil {
		return errors.Wrapf(err, "failed to check if file exists %s", path)
	}
	if exists {
		return nil
	}

	err = ioutil.WriteFile(path, []byte(sampleValuesFile), files.DefaultFileWritePermissions)
	if err != nil {
		return errors.Wrapf(err, "failed to save %s", path)
	}
	return nil
}
