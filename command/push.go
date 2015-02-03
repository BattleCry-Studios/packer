package command

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/hashicorp/atlas-go/archive"
	"github.com/hashicorp/atlas-go/v1"
	"github.com/mitchellh/packer/packer"
)

// archiveTemplateEntry is the name the template always takes within the slug.
const archiveTemplateEntry = ".packer-template"

type PushCommand struct {
	Meta

	client *atlas.Client

	// For tests:
	uploadFn pushUploadFn
}

// pushUploadFn is the callback type used for tests to stub out the uploading
// logic of the push command.
type pushUploadFn func(
	io.Reader, *uploadOpts) (<-chan struct{}, <-chan error, error)

func (c *PushCommand) Run(args []string) int {
	var token string
	var message string

	f := flag.NewFlagSet("push", flag.ContinueOnError)
	f.Usage = func() { c.Ui.Error(c.Help()) }
	f.StringVar(&token, "token", "", "token")
	f.StringVar(&message, "m", "", "message")
	f.StringVar(&message, "message", "", "message")
	if err := f.Parse(args); err != nil {
		return 1
	}

	args = f.Args()
	if len(args) != 1 {
		f.Usage()
		return 1
	}

	// Read the template
	tpl, err := packer.ParseTemplateFile(args[0], nil)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Failed to parse template: %s", err))
		return 1
	}

	// Validate some things
	if tpl.Push.Name == "" {
		c.Ui.Error(fmt.Sprintf(
			"The 'push' section must be specified in the template with\n" +
				"at least the 'name' option set."))
		return 1
	}

	// Determine our token
	if token == "" {
		token = tpl.Push.Token
	}

	// Build our client
	defer func() { c.client = nil }()
	c.client = atlas.DefaultClient()
	if tpl.Push.Address != "" {
		c.client, err = atlas.NewClient(tpl.Push.Address)
		if err != nil {
			c.Ui.Error(fmt.Sprintf(
				"Error setting up API client: %s", err))
			return 1
		}
	}
	if token != "" {
		c.client.Token = token
	}

	// Build the archiving options
	var opts archive.ArchiveOpts
	opts.Include = tpl.Push.Include
	opts.Exclude = tpl.Push.Exclude
	opts.VCS = tpl.Push.VCS
	opts.Extra = map[string]string{
		archiveTemplateEntry: args[0],
	}

	// Determine the path we're archiving. This logic is a bit complicated
	// as there are three possibilities:
	//
	//   1.) BaseDir is an absolute path, just use that.
	//
	//   2.) BaseDir is empty, so we use the directory of the template.
	//
	//   3.) BaseDir is relative, so we use the path relative to the directory
	//       of the template.
	//
	path := tpl.Push.BaseDir
	if path == "" || !filepath.IsAbs(path) {
		tplPath, err := filepath.Abs(args[0])
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error determining path to archive: %s", err))
			return 1
		}
		tplPath = filepath.Dir(tplPath)
		if path != "" {
			tplPath = filepath.Join(tplPath, path)
		}
		path, err = filepath.Abs(tplPath)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("Error determining path to archive: %s", err))
			return 1
		}
	}

	// Find the Atlas post-processors, if possible
	var atlasPPs []packer.RawPostProcessorConfig
	for _, list := range tpl.PostProcessors {
		for _, pp := range list {
			if pp.Type == "atlas" {
				atlasPPs = append(atlasPPs, pp)
			}
		}
	}

	// Build the upload options
	var uploadOpts uploadOpts
	uploadOpts.Slug = tpl.Push.Name
	uploadOpts.Builds = make(map[string]*uploadBuildInfo)
	for _, b := range tpl.Builders {
		info := &uploadBuildInfo{Type: b.Type}

		// Determine if we're artifacting this build
		for _, pp := range atlasPPs {
			if !pp.Skip(b.Name) {
				info.Artifact = true
				break
			}
		}

		uploadOpts.Builds[b.Name] = info
	}

	// Add the upload metadata
	metadata := make(map[string]interface{})
	if message != "" {
		metadata["message"] = message
	}
	uploadOpts.Metadata = metadata

	// Warn about builds not having post-processors.
	var badBuilds []string
	for name, b := range uploadOpts.Builds {
		if b.Artifact {
			continue
		}

		badBuilds = append(badBuilds, name)
	}
	if len(badBuilds) > 0 {
		c.Ui.Error(fmt.Sprintf(
			"Warning! One or more of the builds in this template does not\n"+
				"have an Atlas post-processor. Artifacts from this template will\n"+
				"not appear in the Atlas artifact registry.\n\n"+
				"This is just a warning. Atlas will still build your template\n"+
				"and assume other post-processors are sending the artifacts where\n"+
				"they need to go.\n\n"+
				"Builds: %s\n\n", strings.Join(badBuilds, ", ")))
	}

	// Start the archiving process
	r, err := archive.CreateArchive(path, &opts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error archiving: %s", err))
		return 1
	}
	defer r.Close()

	// Start the upload process
	doneCh, uploadErrCh, err := c.upload(r, &uploadOpts)
	if err != nil {
		c.Ui.Error(fmt.Sprintf("Error starting upload: %s", err))
		return 1
	}

	// Make a ctrl-C channel
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	err = nil
	select {
	case err = <-uploadErrCh:
		err = fmt.Errorf("Error uploading: %s", err)
	case <-sigCh:
		err = fmt.Errorf("Push cancelled from Ctrl-C")
	case <-doneCh:
	}

	if err != nil {
		c.Ui.Error(err.Error())
		return 1
	}

	c.Ui.Output(fmt.Sprintf("Push successful to '%s'", tpl.Push.Name))
	return 0
}

func (*PushCommand) Help() string {
	helpText := `
Usage: packer push [options] TEMPLATE

  Push the given template and supporting files to a Packer build service such as
  Atlas.

  If a build configuration for the given template does not exist, it will be
  created automatically. If the build configuration already exists, a new
  version will be created with this template and the supporting files.

  Additional configuration options (such as the Atlas server URL and files to
  include) may be specified in the "push" section of the Packer template. Please
  see the online documentation for more information about these configurables.

Options:

  -m, -message=<detail>    A message to identify the purpose or changes in this
                           Packer template much like a VCS commit message

  -token=<token>           The access token to use to when uploading
`

	return strings.TrimSpace(helpText)
}

func (*PushCommand) Synopsis() string {
	return "push a template and supporting files to a Packer build service"
}

func (c *PushCommand) upload(
	r *archive.Archive, opts *uploadOpts) (<-chan struct{}, <-chan error, error) {
	if c.uploadFn != nil {
		return c.uploadFn(r, opts)
	}

	// Separate the slug into the user and name components
	user, name, err := atlas.ParseSlug(opts.Slug)
	if err != nil {
		return nil, nil, fmt.Errorf("upload: %s", err)
	}

	// Get the build configuration
	bc, err := c.client.BuildConfig(user, name)
	if err != nil {
		if err == atlas.ErrNotFound {
			// Build configuration doesn't exist, attempt to create it
			bc, err = c.client.CreateBuildConfig(user, name)
		}

		if err != nil {
			return nil, nil, fmt.Errorf("upload: %s", err)
		}
	}

	// Build the version to send up
	version := atlas.BuildConfigVersion{
		User:   bc.User,
		Name:   bc.Name,
		Builds: make([]atlas.BuildConfigBuild, 0, len(opts.Builds)),
	}
	for name, info := range opts.Builds {
		version.Builds = append(version.Builds, atlas.BuildConfigBuild{
			Name:     name,
			Type:     info.Type,
			Artifact: info.Artifact,
		})
	}

	// Start the upload
	doneCh, errCh := make(chan struct{}), make(chan error)
	go func() {
		err := c.client.UploadBuildConfigVersion(&version, opts.Metadata, r, r.Size)
		if err != nil {
			errCh <- err
			return
		}

		close(doneCh)
	}()

	return doneCh, errCh, nil
}

type uploadOpts struct {
	URL      string
	Slug     string
	Builds   map[string]*uploadBuildInfo
	Metadata map[string]interface{}
}

type uploadBuildInfo struct {
	Type     string
	Artifact bool
}
