package config

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"

	"github.com/updatecli/updatecli/pkg/core/pipeline/action"
	"github.com/updatecli/updatecli/pkg/core/pipeline/autodiscovery"
	"github.com/updatecli/updatecli/pkg/core/pipeline/condition"
	"github.com/updatecli/updatecli/pkg/core/pipeline/scm"
	"github.com/updatecli/updatecli/pkg/core/pipeline/source"
	"github.com/updatecli/updatecli/pkg/core/pipeline/target"
	"github.com/updatecli/updatecli/pkg/core/result"
	"github.com/updatecli/updatecli/pkg/core/text"
	"github.com/updatecli/updatecli/pkg/core/version"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/updatecli/updatecli/pkg/plugins/utils/gitgeneric"
)

const (
	// LOCALSCMIDENTIFIER defines the scm id used to configure the local scm directory
	LOCALSCMIDENTIFIER string = "local"
	// DefaultConfigFilename defines the default updatecli configuration filename
	DefaultConfigFilename string = "updatecli.yaml"
	// DefaultConfigDirname defines the default updatecli manifest directory
	DefaultConfigDirname string = "updatecli.d"
)

// Config contains cli configuration
type Config struct {
	// filename contains the updatecli manifest filename
	filename string
	// Spec describe an updatecli manifest
	Spec Spec
	// gitHandler holds a git client implementation to manipulate git SCMs
	gitHandler gitgeneric.GitHandler
}

// Spec contains pipeline configuration
type Spec struct {
	// Name defines a pipeline name
	Name string `yaml:",omitempty" jsonschema:"required"`
	// PipelineID allows to identify a full pipeline run, this value is propagated into each target if not defined at that level
	PipelineID string `yaml:",omitempty"`
	// AutoDiscovery defines parameters to the autodiscovery feature
	AutoDiscovery autodiscovery.Config `yaml:",omitempty"`
	// Title is used for the full pipeline
	Title string `yaml:",omitempty"`
	// !Deprecated in favor of `actions`
	PullRequests map[string]action.Config `yaml:",omitempty" jsonschema:"-"`
	// Actions defines the list of action configurations which need to be managed
	Actions map[string]action.Config `yaml:",omitempty"`
	// SCMs defines the list of repository configuration used to fetch content from.
	SCMs map[string]scm.Config `yaml:"scms,omitempty"`
	// Sources defines the list of source configuration
	Sources map[string]source.Config `yaml:",omitempty"`
	// Conditions defines the list of condition configuration
	Conditions map[string]condition.Config `yaml:",omitempty"`
	// Targets defines the list of target configuration
	Targets map[string]target.Config `yaml:",omitempty"`
	// Version specifies the minimum updatecli version compatible with the manifest
	Version string `yaml:",omitempty"`
}

// Option contains configuration options such as filepath located on disk,etc.
type Option struct {
	// ManifestFile contains the updatecli manifest full file path
	ManifestFile string
	// ValuesFiles contains the list of updatecli values full file path
	ValuesFiles []string
	// SecretsFiles contains the list of updatecli sops secrets full file path
	SecretsFiles []string
	// DisableTemplating specifies if needs to be done
	DisableTemplating bool
}

// Reset reset configuration
func (config *Config) Reset() {
	*config = Config{
		gitHandler: gitgeneric.GoGit{},
	}
}

// New reads an updatecli configuration file
func New(option Option) (configs []Config, err error) {

	dirname, basename := filepath.Split(option.ManifestFile)

	// We need to be sure to generate a file checksum before we inject
	// templates values as in some situation those values changes for each run
	pipelineID, err := FileChecksum(option.ManifestFile)
	if err != nil {
		return configs, err
	}

	logrus.Infof("Loading Pipeline %q", option.ManifestFile)

	// Load updatecli manifest no matter the file extension
	c, err := os.Open(option.ManifestFile)

	if err != nil {
		return configs, err
	}

	defer c.Close()

	var templatedManifestContent []byte
	rawManifestContent, err := io.ReadAll(c)
	if err != nil {
		return configs, err
	}

	specs := []Spec{}

	if !option.DisableTemplating {
		// Try to template manifest no matter the extension
		// templated manifest must respect its extension before and after templating

		cwd, err := os.Getwd()
		if err != nil {
			return configs, err
		}
		fs := os.DirFS(cwd)

		t := Template{
			CfgFile:      filepath.Join(dirname, basename),
			ValuesFiles:  option.ValuesFiles,
			SecretsFiles: option.SecretsFiles,
			fs:           fs,
		}

		templatedManifestContent, err = t.New(rawManifestContent)
		if err != nil {
			logrus.Errorf("Error while templating %q:\n---\n%s\n---\n\t%s\n", option.ManifestFile, string(rawManifestContent), err.Error())
			return configs, err
		}

		if GolangTemplatingDiff {
			diff := text.Diff("raw manifest", "templated manifest", string(rawManifestContent), string(templatedManifestContent))
			switch diff {
			case "":
				logrus.Debugln("no Golang templating detected")
			default:
				logrus.Debugf("Golang templating change detected:\n%s\n\n---\n", diff)
			}
		}
	}

	switch extension := filepath.Ext(basename); extension {
	case ".tpl", ".tmpl", ".yaml", ".yml", ".json":

		err = unmarshalConfigSpec(templatedManifestContent, &specs)
		if err != nil {
			return configs, err
		}

	default:
		logrus.Debugf("file extension '%s' not supported for file '%s'", extension, option.ManifestFile)
		return configs, ErrConfigFileTypeNotSupported
	}

	configs = make([]Config, len(specs))
	for id := range specs {

		configs[id].Reset()
		configs[id].filename = option.ManifestFile
		configs[id].Spec = specs[id]
		// config.PipelineID is required for config.Validate()
		if len(configs[id].Spec.PipelineID) == 0 {
			configs[id].Spec.PipelineID = pipelineID
		}

		// By default Set config.Version to the current updatecli version
		if len(configs[id].Spec.Version) == 0 {
			configs[id].Spec.Version = version.Version
		}

		// Ensure there is a local SCM defined as specified
		if err = configs[id].EnsureLocalScm(); err != nil {
			logrus.Errorf("Skipping manifest in position %q from %q: %s", id, option.ManifestFile, err.Error())
			continue
		}

		/** Check for deprecated directives **/
		// pullrequests deprecated over actions
		if len(configs[id].Spec.PullRequests) > 0 {
			if len(configs[id].Spec.Actions) > 0 {
				err := fmt.Errorf("the `pullrequests` and `actions` keywords are mutually exclusive. Please use only `actions` as `pullrequests` is deprecated")
				logrus.Errorf("Skipping manifest in position %q from %q: %s", id, option.ManifestFile, err.Error())
				continue
			}

			logrus.Warningf("The `pullrequests` keyword is deprecated in favor of `actions`, please update this manifest. Updatecli will continue the execution while trying to translate `pullrequests` to `actions`.")

			configs[id].Spec.Actions = configs[id].Spec.PullRequests
			configs[id].Spec.PullRequests = nil
		}

		err = configs[id].Validate()
		if err != nil {
			logrus.Errorf("Skipping manifest in position %q from %q: %s", id, option.ManifestFile, err.Error())
			continue
		}

		if len(configs[id].Spec.Name) == 0 {
			configs[id].Spec.Name = strings.ToTitle(basename)
		}

		err = configs[id].Validate()
		if err != nil {
			logrus.Errorf("Skipping manifest in position %q from %q: %s", id, option.ManifestFile, err.Error())
			continue
		}
	}

	return configs, err

}

// IsManifestDifferentThanOnDisk checks if an Updatecli manifest in memory is the same than the one on disk
func (c *Config) IsManifestDifferentThanOnDisk() (bool, error) {

	buf := bytes.NewBufferString("")

	encoder := yaml.NewEncoder(buf)

	defer func() {
		err := encoder.Close()
		if err != nil {
			logrus.Errorln(err)
		}
	}()

	encoder.SetIndent(YAMLSetIdent)

	err := encoder.Encode(c.Spec)

	if err != nil {
		return false, err
	}

	data := buf.Bytes()

	onDiskData, err := os.ReadFile(c.filename)
	if err != nil {
		return false, err
	}

	if string(onDiskData) == string(data) {
		logrus.Infof("%s No Updatecli manifest change required", result.SUCCESS)
		return false, nil
	}

	edits := myers.ComputeEdits(span.URIFromPath(c.filename), string(onDiskData), string(data))
	diff := fmt.Sprint(gotextdiff.ToUnified(c.filename+"(old)", c.filename+"(updated)", string(onDiskData), edits))

	logrus.Infof("%s Updatecli manifest change required\n%s", result.ATTENTION, diff)

	return true, nil

}

// SaveOnDisk saves an updatecli manifest to disk
func (c *Config) SaveOnDisk() error {

	file, err := os.OpenFile(c.filename, os.O_WRONLY, os.ModePerm)
	if err != nil {
		return err
	}

	defer func() {
		err = file.Close()
		if err != nil {
			logrus.Errorln(err)
		}
	}()

	encoder := yaml.NewEncoder(file)

	defer func() {
		err = encoder.Close()
		if err != nil {
			logrus.Errorln(err)
		}
	}()

	// Must be set to 4 so we align with the default behavior when
	// we marshaled the inmemory yaml to compare with the one from disk
	encoder.SetIndent(YAMLSetIdent)
	err = encoder.Encode(c.Spec)

	if err != nil {
		return err
	}

	return nil
}

// Display shows updatecli configuration including secrets !
func (config *Config) Display() error {
	c, err := yaml.Marshal(&config.Spec)
	if err != nil {
		return err
	}
	logrus.Infof("%s", string(c))

	return nil
}

func (config *Config) validateActions() error {
	for id, a := range config.Spec.Actions {
		if err := a.Validate(); err != nil {
			logrus.Errorf("bad parameters for action %q", id)
			return err
		}

		// Then validate that the action specifies an existing SCM
		if len(a.ScmID) > 0 {
			if _, ok := config.Spec.SCMs[a.ScmID]; !ok {
				logrus.Errorf("The action %q specifies a scm id %q which does not exist", id, a.ScmID)
				return ErrBadConfig
			}
		}

		// a.Validate may modify the object during validation
		// so we want to be sure that we save those modifications
		config.Spec.Actions[id] = a
	}
	return nil
}

func (config *Config) validateAutodiscovery() error {
	// Then validate that the action specifies an existing SCM
	if len(config.Spec.AutoDiscovery.ScmId) > 0 {
		if _, ok := config.Spec.SCMs[config.Spec.AutoDiscovery.ScmId]; !ok {
			logrus.Errorf("The autodiscovery specifies a scm id %q which does not exist",
				config.Spec.AutoDiscovery.ScmId)
			return ErrBadConfig
		}
	}

	if err := config.Spec.AutoDiscovery.GroupBy.Validate(); err != nil {
		return ErrBadConfig
	}

	return nil
}

func (config *Config) validateSources() error {
	for id, s := range config.Spec.Sources {
		err := s.Validate()
		if err != nil {
			logrus.Errorf("bad parameters for source %q", id)
			return ErrBadConfig
		}

		if IsTemplatedString(id) {
			logrus.Errorf("sources key %q contains forbidden go template instruction", id)
			return ErrNotAllowedTemplatedKey
		}

		// s.Validate may modify the object during validation
		// so we want to be sure that we save those modifications
		config.Spec.Sources[id] = s
	}
	return nil

}

func (config *Config) validateSCMs() error {
	for id, scmConfig := range config.Spec.SCMs {
		if err := scmConfig.Validate(); err != nil {
			logrus.Errorf("bad parameter(s) for scm %q", id)
			return err
		}
	}
	return nil
}

func (config *Config) validateTargets() error {

	for id, t := range config.Spec.Targets {

		err := t.Validate()
		if err != nil {
			logrus.Errorf("bad parameters for target %q", id)
			return ErrBadConfig
		}

		if len(t.SourceID) > 0 {
			if _, ok := config.Spec.Sources[t.SourceID]; !ok {
				logrus.Errorf("the specified sourceid %q for condition[id] does not exist", t.SourceID)
				return ErrBadConfig
			}
		}

		// Only check/guess the sourceID if the user did not disable it (default is enabled)
		if !t.DisableSourceInput {
			// Try to guess SourceID
			if len(t.SourceID) == 0 && len(config.Spec.Sources) > 1 {

				logrus.Errorf("empty 'sourceid' for target %q", id)
				return ErrBadConfig
			} else if len(t.SourceID) == 0 && len(config.Spec.Sources) == 1 {
				for id := range config.Spec.Sources {
					t.SourceID = id
				}
			}
		}

		if IsTemplatedString(id) {
			logrus.Errorf("target key %q contains forbidden go template instruction", id)
			return ErrNotAllowedTemplatedKey
		}

		// t.Validate may modify the object during validation
		// so we want to be sure that we save those modifications
		config.Spec.Targets[id] = t

	}
	return nil
}

func (config *Config) validateConditions() error {
	for id, c := range config.Spec.Conditions {
		err := c.Validate()
		if err != nil {
			logrus.Errorf("bad parameters for condition %q", id)
			return ErrBadConfig
		}

		if len(c.SourceID) > 0 {
			if _, ok := config.Spec.Sources[c.SourceID]; !ok {
				logrus.Errorf("the specified sourceid %q for condition[id] does not exist", c.SourceID)
				return ErrBadConfig
			}
		}
		// Only check/guess the sourceID if the user did not disable it (default is enabled)
		if !c.DisableSourceInput {
			// Try to guess SourceID
			if len(c.SourceID) == 0 && len(config.Spec.Sources) > 1 {
				logrus.Errorf("The condition %q has an empty 'sourceid' attribute.", id)
				return ErrBadConfig
			} else if len(c.SourceID) == 0 && len(config.Spec.Sources) == 1 {
				for id := range config.Spec.Sources {
					c.SourceID = id
				}
			}
		}

		if IsTemplatedString(id) {
			logrus.Errorf("condition key %q contains forbidden go template instruction", id)
			return ErrNotAllowedTemplatedKey
		}

		config.Spec.Conditions[id] = c

	}
	return nil
}

// Validate run various validation test on the configuration and update fields if necessary
func (config *Config) Validate() error {

	var errs []error
	err := config.ValidateManifestCompatibility()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("updatecli version compatibility error:\n%s", err))
	}

	err = config.validateConditions()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("conditions validation error:\n%s", err))
	}

	err = config.validateActions()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("actions validation error:\n%s", err))
	}

	err = config.validateAutodiscovery()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("autodiscovery validation error:\n%s", err))
	}

	err = config.validateSCMs()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("scms validation error:\n%s", err))
	}

	err = config.validateSources()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("sources validation error:\n%s", err))
	}

	err = config.validateTargets()
	if err != nil {
		errs = append(
			errs,
			fmt.Errorf("targets validation error:\n%s", err))
	}

	// Concatenate error message
	if len(errs) > 0 {
		err = errs[0]
		for i := range errs {
			if i > 1 {
				err = fmt.Errorf("%s\n%s", err, errs[i])
			}
		}
		return err
	}

	return nil
}

func (config *Config) ValidateManifestCompatibility() error {
	isCompatibleUpdatecliVersion, err := version.IsGreaterThan(
		version.Version,
		config.Spec.Version)

	if err != nil {
		return fmt.Errorf("pipeline %q - %q", config.Spec.Name, err)
	}

	// Ensure that the current updatecli version is compatible with the manifest
	if !isCompatibleUpdatecliVersion {
		return fmt.Errorf("pipeline %q requires Updatecli version greater than %q, skipping", config.Spec.Name, config.Spec.Version)
	}

	return nil
}

// Update updates its own configuration file
// It's used when the configuration expected a value defined a runtime
func (config *Config) Update(data interface{}) (err error) {

	content, err := yaml.Marshal(config.Spec)
	if err != nil {
		return err
	}

	tmpl, err := template.New("cfg").Funcs(updatecliRuntimeFuncMap(data)).Parse(string(content))
	if err != nil {
		return err
	}

	b := bytes.Buffer{}
	if err := tmpl.Execute(&b, &data); err != nil {
		return err
	}

	err = yaml.Unmarshal(b.Bytes(), &config.Spec)
	if err != nil {
		return err
	}

	err = config.Validate()
	if err != nil {
		return err
	}

	return err
}
