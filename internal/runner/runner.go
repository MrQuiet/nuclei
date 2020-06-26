package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/d5/tengo/v2"
	"github.com/karrick/godirwalk"
	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/nuclei/pkg/executor"
	"github.com/projectdiscovery/nuclei/pkg/requests"
	"github.com/projectdiscovery/nuclei/pkg/templates"
	"github.com/projectdiscovery/nuclei/pkg/workflows"
)

// Runner is a client for running the enumeration process.
type Runner struct {
	// output is the output file to write if any
	output      *os.File
	outputMutex *sync.Mutex

	tempFile        string
	templatesConfig *nucleiConfig
	// options contains configuration options for runner
	options *Options
}

// New creates a new client for running enumeration process.
func New(options *Options) (*Runner, error) {
	runner := &Runner{
		outputMutex: &sync.Mutex{},
		options:     options,
	}

	if err := runner.updateTemplates(); err != nil {
		return nil, err
	}
	if (options.Templates == "" || options.Targets == "" && !options.Stdin) && options.UpdateTemplates {
		os.Exit(0)
	}

	// If we have stdin, write it to a new file
	if options.Stdin {
		tempInput, err := ioutil.TempFile("", "stdin-input-*")
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(tempInput, os.Stdin); err != nil {
			return nil, err
		}
		runner.tempFile = tempInput.Name()
		tempInput.Close()
	}

	// Create the output file if asked
	if options.Output != "" {
		output, err := os.Create(options.Output)
		if err != nil {
			gologger.Fatalf("Could not create output file '%s': %s\n", options.Output, err)
		}
		runner.output = output
	}
	return runner, nil
}

// Close releases all the resources and cleans up
func (r *Runner) Close() {
	r.output.Close()
	os.Remove(r.tempFile)
}

// RunEnumeration sets up the input layer for giving input nuclei.
// binary and runs the actual enumeration
func (r *Runner) RunEnumeration() {
	// Single yaml provided
	if !strings.Contains(r.options.Templates, "*") && strings.HasSuffix(r.options.Templates, ".yaml") {
		t := r.parse(r.options.Templates)
		switch t.(type) {
		case *templates.Template:
			results := false
			template := t.(*templates.Template)
			for _, request := range template.RequestsHTTP {
				results = r.processTemplateRequest(template, request)
			}
			for _, request := range template.RequestsDNS {
				dnsResults := r.processTemplateRequest(template, request)
				if !results {
					results = dnsResults
				}
			}
			if !results {
				if r.output != nil {
					outputFile := r.output.Name()
					r.output.Close()
					os.Remove(outputFile)
				}
				gologger.Infof("No results found for the template. Happy hacking!")
			}
		case *workflows.Workflow:
			workflow := t.(*workflows.Workflow)
			r.ProcessWorkflowWithList(workflow)
		default:
			gologger.Errorf("Could not parse file '%s'\n", r.options.Templates)
		}
		return
	}

	// If the template path is glob
	if strings.Contains(r.options.Templates, "*") {
		// Handle the glob, evaluate it and run all the template file checks
		matches, err := filepath.Glob(r.options.Templates)
		if err != nil {
			gologger.Fatalf("Could not evaluate template path '%s': %s\n", r.options.Templates, err)
		}

		var results bool
		for _, match := range matches {
			t := r.parse(match)
			switch t.(type) {
			case *templates.Template:
				template := t.(*templates.Template)
				for _, request := range template.RequestsDNS {
					dnsResults := r.processTemplateRequest(template, request)
					if dnsResults {
						results = dnsResults
					}
				}
				for _, request := range template.RequestsHTTP {
					httpResults := r.processTemplateRequest(template, request)
					if httpResults {
						results = httpResults
					}
				}
			case *workflows.Workflow:
				workflow := t.(*workflows.Workflow)
				r.ProcessWorkflowWithList(workflow)
			default:
				gologger.Errorf("Could not parse file '%s'\n", r.options.Templates)
			}
		}
		if !results {
			if r.output != nil {
				outputFile := r.output.Name()
				r.output.Close()
				os.Remove(outputFile)
			}
			gologger.Infof("No results found for the templates. Happy hacking!")
		}
		return
	}
	// If the template passed is a directory
	matches := []string{}
	// Recursively walk down the Templates directory and run all the template file checks
	err := godirwalk.Walk(r.options.Templates, &godirwalk.Options{
		Callback: func(path string, d *godirwalk.Dirent) error {
			if !d.IsDir() && strings.HasSuffix(path, ".yaml") {
				matches = append(matches, path)
			}
			return nil
		},
		ErrorCallback: func(path string, err error) godirwalk.ErrorAction {
			return godirwalk.SkipNode
		},
		Unsorted: true,
	})
	if err != nil {
		gologger.Fatalf("Could not find templates in directory '%s': %s\n", r.options.Templates, err)
	}
	// 0 matches means no templates were found in directory
	if len(matches) == 0 {
		gologger.Fatalf("Error, no templates found in directory: '%s'\n", r.options.Templates)
	}
	var results bool
	for _, match := range matches {
		t := r.parse(match)
		switch t.(type) {
		case *templates.Template:
			template := t.(*templates.Template)

			for _, request := range template.RequestsDNS {
				dnsResults := r.processTemplateRequest(template, request)
				if dnsResults {
					results = dnsResults
				}
			}
			for _, request := range template.RequestsHTTP {
				httpResults := r.processTemplateRequest(template, request)
				if httpResults {
					results = httpResults
				}
			}
		case *workflows.Workflow:
			workflow := t.(*workflows.Workflow)
			r.ProcessWorkflowWithList(workflow)
		default:
			gologger.Errorf("Could not parse file '%s'\n", r.options.Templates)
		}
	}
	if !results {
		if r.output != nil {
			outputFile := r.output.Name()
			r.output.Close()
			os.Remove(outputFile)
		}
		gologger.Infof("No results found for the template. Happy hacking!")
	}
	return
}

// processTemplate processes a template and runs the enumeration on all the targets
func (r *Runner) processTemplateRequest(template *templates.Template, request interface{}) bool {
	var file *os.File
	var err error

	// Handle a list of hosts as argument
	if r.options.Targets != "" {
		file, err = os.Open(r.options.Targets)
	} else if r.options.Stdin {
		file, err = os.Open(r.tempFile)
	}
	if err != nil {
		gologger.Fatalf("Could not open targets file '%s': %s\n", r.options.Targets, err)
	}
	results := r.processTemplateWithList(template, request, file)
	file.Close()
	return results
}

// processDomain processes the list with a template
func (r *Runner) processTemplateWithList(template *templates.Template, request interface{}, reader io.Reader) bool {
	// Display the message for the template
	message := fmt.Sprintf("[%s] Loaded template %s (@%s)", template.ID, template.Info.Name, template.Info.Author)
	if template.Info.Severity != "" {
		message += " [" + template.Info.Severity + "]"
	}
	gologger.Infof("%s\n", message)

	var writer *bufio.Writer
	if r.output != nil {
		writer = bufio.NewWriter(r.output)
		defer writer.Flush()
	}

	var httpExecutor *executor.HTTPExecutor
	var dnsExecutor *executor.DNSExecutor
	var err error

	// Create an executor based on the request type.
	switch value := request.(type) {
	case *requests.DNSRequest:
		dnsExecutor = executor.NewDNSExecutor(&executor.DNSOptions{
			Debug:      r.options.Debug,
			Template:   template,
			DNSRequest: value,
			Writer:     writer,
		})
	case *requests.HTTPRequest:
		httpExecutor, err = executor.NewHTTPExecutor(&executor.HTTPOptions{
			Debug:         r.options.Debug,
			Template:      template,
			HTTPRequest:   value,
			Writer:        writer,
			Timeout:       r.options.Timeout,
			Retries:       r.options.Retries,
			ProxyURL:      r.options.ProxyURL,
			ProxySocksURL: r.options.ProxySocksURL,
			CustomHeaders: r.options.CustomHeaders,
		})
	}
	if err != nil {
		gologger.Warningf("Could not create http client: %s\n", err)
		return false
	}

	limiter := make(chan struct{}, r.options.Threads)
	wg := &sync.WaitGroup{}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			continue
		}
		limiter <- struct{}{}
		wg.Add(1)

		go func(URL string) {
			var err error

			if httpExecutor != nil {
				err = httpExecutor.ExecuteHTTP(URL)
			}
			if dnsExecutor != nil {
				err = dnsExecutor.ExecuteDNS(URL)
			}
			if err != nil {
				gologger.Warningf("Could not execute step: %s\n", err)
			}
			<-limiter
			wg.Done()
		}(text)
	}
	close(limiter)
	wg.Wait()

	// See if we got any results from the executors
	var results bool
	if httpExecutor != nil {
		results = httpExecutor.GotResults()
	}
	if dnsExecutor != nil {
		if !results {
			results = dnsExecutor.GotResults()
		}
	}
	return results
}

// ProcessWorkflowWithList coming from stdin or list of targets
func (r *Runner) ProcessWorkflowWithList(workflow *workflows.Workflow) {
	var file *os.File
	var err error
	// Handle a list of hosts as argument
	if r.options.Targets != "" {
		file, err = os.Open(r.options.Targets)
	} else if r.options.Stdin {
		file, err = os.Open(r.tempFile)
	}
	if err != nil {
		gologger.Fatalf("Could not open targets file '%s': %s\n", r.options.Targets, err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			continue
		}
		r.ProcessWorkflow(workflow, text)
	}
}

// ProcessWorkflow towards an URL
func (r *Runner) ProcessWorkflow(workflow *workflows.Workflow, URL string) error {
	script := tengo.NewScript([]byte(workflow.Logic))
	for name, value := range workflow.Variables {
		var writer *bufio.Writer
		if r.output != nil {
			writer = bufio.NewWriter(r.output)
			defer writer.Flush()
		}
		templatePath := path.Join(r.options.TemplatesDirectory, value)
		template, err := templates.ParseTemplate(templatePath)
		if err != nil {
			gologger.Errorf("Could not parse template file '%s': %s\n", value, err)
			return err
		}

		httpOptions := &executor.HTTPOptions{
			Debug:         r.options.Debug,
			Writer:        writer,
			Template:      template,
			Timeout:       r.options.Timeout,
			Retries:       r.options.Retries,
			ProxyURL:      r.options.ProxyURL,
			ProxySocksURL: r.options.ProxySocksURL,
			CustomHeaders: r.options.CustomHeaders,
		}
		script.Add(name, &workflows.NucleiVar{Options: httpOptions, URL: URL})
	}

	_, err := script.RunContext(context.Background())
	if err != nil {
		gologger.Errorf("Could not execute workflow '%s': %s\n", workflow.ID, err)
		return err
	}

	// OUTPUT - TODO - Any suggestion?

	return nil
}

func (r *Runner) parse(file string) interface{} {
	// check if it's a template
	template, errTemplate := templates.ParseTemplate(r.options.Templates)
	if errTemplate == nil {
		return template
	}

	// check if it's a workflow
	workflow, errWorkflow := workflows.Parse(r.options.Templates)
	if errWorkflow == nil {
		return workflow
	}

	return nil
}
