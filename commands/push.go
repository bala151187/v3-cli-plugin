package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"code.cloudfoundry.org/cli/cf/appfiles"
	"code.cloudfoundry.org/cli/plugin"
	"code.cloudfoundry.org/gofileutils/fileutils"
	. "github.com/cloudfoundry/v3-cli-plugin/models"
	. "github.com/cloudfoundry/v3-cli-plugin/util"
	"github.com/simonleung8/flags"
)

func Push(cliConnection plugin.CliConnection, args []string) {
	appDir := "."
	buildpack := "null"
	dockerImage := ""
	verbose := false

	fc := flags.New()
	fc.NewBoolFlag("verbose", "vb", "verbose")
	fc.NewStringFlag("filepath", "p", "path to app dir or zip to upload")
	fc.NewStringFlag("buildpack", "b", "the buildpack to use")
	fc.NewStringFlag("docker-image", "di", "the docker image to use")
	fc.Parse(args...)
	if fc.IsSet("p") {
		appDir = fc.String("p")
	}
	if fc.IsSet("b") {
		buildpack = fmt.Sprintf(`"%s"`, fc.String("b"))
	}
	if fc.IsSet("vb") {
		verbose = true
	}
	if fc.IsSet("di") {
		dockerImage = fmt.Sprintf(`"%s"`, fc.String("di"))
	}

	mySpace, _ := cliConnection.GetCurrentSpace()

	lifecycle := ""
	if dockerImage != "" {
		lifecycle = `"lifecycle": { "type": "docker", "data": {} }`
	} else {
		lifecycle = fmt.Sprintf(`"lifecycle": { "type": "buildpack", "data": { "buildpacks": [%s] } }`, buildpack)
	}

	//create the app
	rawOutput, err := cliConnection.CliCommandWithoutTerminalOutput("curl", "/v3/apps", "-X", "POST", "-d",
		fmt.Sprintf(`{"name":"%s", "relationships": { "space": { "data": {"guid":"%s"}}}, %s}`, fc.Args()[1], mySpace.Guid, lifecycle))
	FreakOut(err)
	output := strings.Join(rawOutput, "")
	app := V3AppModel{}
	err = json.Unmarshal([]byte(output), &app)
	FreakOut(err)
	if app.Error_Code != "" {
		FreakOut(errors.New("Error creating v3 app: " + app.Error_Code))
	}
	if len(app.Errors) > 0 {
		FreakOut(fmt.Errorf("Error in /v3/apps: %s", app.Errors))
	}
	time.Sleep(2 * time.Second) // wait for app to settle before kicking off the log streamer
	go Logs(cliConnection, args)
	time.Sleep(2 * time.Second) // b/c sharing the cliConnection makes things break

	//create package
	pack := V3PackageModel{}
	if dockerImage != "" {
		request := fmt.Sprintf(`{"type": "docker", "data": {"image": %s}, "relationships": {"app": {"data": {"guid": "%s"}}}}`, dockerImage, app.Guid)
		if verbose {
			fmt.Printf("... /v3/packages -X POST -d %s\n", request)
		}
		rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", "/v3/packages", "-X", "POST", "-d", request)
		FreakOut(err)
		output = strings.Join(rawOutput, "")

		err = json.Unmarshal([]byte(output), &pack)
		if err != nil {
			FreakOut(errors.New("Error creating v3 app package: " + app.Error_Code))
		}
		if len(pack.Errors) > 0 {
			FreakOut(fmt.Errorf("POST /v3/packages (docker): %s", pack.Errors))
		}
	} else {
		//create the empty package to upload the app bits to
		request := fmt.Sprintf(`{"type": "bits", "relationships": {"app": {"data": {"guid": "%s"}}}}`, app.Guid)
		if verbose {
			fmt.Printf("... /v3/packages -X POST -d %s\n", request)
		}
		rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", "/v3/packages", "-X", "POST", "-d", request)
		FreakOut(err)
		output = strings.Join(rawOutput, "")

		err = json.Unmarshal([]byte(output), &pack)
		if err != nil {
			FreakOut(errors.New("Error creating v3 app package: " + app.Error_Code))
		}
		if len(pack.Errors) > 0 {
			FreakOut(fmt.Errorf("POST /v3/packages (bits): %s", pack.Errors))
		}

		token, err := cliConnection.AccessToken()
		FreakOut(err)
		api, apiErr := cliConnection.ApiEndpoint()
		FreakOut(apiErr)
		apiString := fmt.Sprintf("%s", api)
		if strings.Index(apiString, "s") == 4 {
			apiString = apiString[:4] + apiString[5:]
		}

		//gather files
		zipper := appfiles.ApplicationZipper{}
		fileutils.TempFile("uploads", func(zipFile *os.File, err error) {
			zipper.Zip(appDir, zipFile)
			if verbose {
				fmt.Printf("... /v3/packages/%s/upload ...\n", pack.Guid)
			}
			_, upload := exec.Command("curl", fmt.Sprintf("%s/v3/packages/%s/upload", apiString, pack.Guid), "-F", fmt.Sprintf("bits=@%s", zipFile.Name()), "-H", fmt.Sprintf("Authorization: %s", token), "-H", "Expect:").Output()
			FreakOut(upload)
		})

		//waiting for cc to pour bits into blobstore
		Poll(cliConnection, fmt.Sprintf("/v3/packages/%s", pack.Guid), "READY", 5*time.Minute, "Package failed to upload")
	}

	buildPostRequest := fmt.Sprintf(`{ %s, "package": { "guid": "%s" }}`, lifecycle, pack.Guid)

	if verbose {
		fmt.Printf("... /v3/builds -X POST -d %s\n", buildPostRequest)
	}
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", "/v3/builds", "-X", "POST", "-d", buildPostRequest)
	FreakOut(err)
	output = strings.Join(rawOutput, "")
	build := V3BuildModel{}
	err = json.Unmarshal([]byte(output), &build)
	if err != nil {
		FreakOut(errors.New("error marshaling the v3 build: " + err.Error()))
	}
	if len(build.Errors) > 0 {
		FreakOut(fmt.Errorf("Error in /v3/builds: %s (request: %s)", build.Errors, buildPostRequest))
	}
	//wait for the build to be ready
	if verbose {
		fmt.Printf("... wait for the build to be ready: /v3/builds/%s\n", build.Guid)
	}
	PollWithBadString(cliConnection, fmt.Sprintf("/v3/builds/%s", build.Guid), "STAGED", "FAILED", 10*time.Minute, "Build failed to stage")

	//get the droplet from the build
	if verbose {
		fmt.Printf("... /v3/builds/%s\n", build.Guid)
	}
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", fmt.Sprintf("/v3/builds/%s", build.Guid))
	FreakOut(err)
	output = strings.Join(rawOutput, "")
	build = V3BuildModel{}
	err = json.Unmarshal([]byte(output), &build)
	if err != nil {
		FreakOut(errors.New("error marshaling the v3 build: " + err.Error()))
	}
	if len(build.Errors) > 0 {
		FreakOut(fmt.Errorf("Error in /v3/builds: %s (request: %s)", build.Errors, buildPostRequest))
	}
	droplet := build.Droplet

	//assign droplet to the app
	if verbose {
		fmt.Printf("... /v3/apps/%s/relationships/current_droplet", app.Guid)
	}
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", fmt.Sprintf("/v3/apps/%s/relationships/current_droplet", app.Guid), "-X", "PATCH", "-d", fmt.Sprintf("{\"data\": {\"guid\":\"%s\"}}", droplet.Guid))
	FreakOut(err)
	output = strings.Join(rawOutput, "")

	//pick the first available shared domain, get the guid
	space, _ := cliConnection.GetCurrentSpace()
	nextUrl := "/v2/shared_domains"
	allDomains := DomainsModel{}
	for nextUrl != "" {
		if verbose {
			fmt.Printf("%s", nextUrl)
		}
		rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", nextUrl)
		FreakOut(err)
		output = strings.Join(rawOutput, "")
		domains := DomainsModel{}
		err = json.Unmarshal([]byte(output), &domains)
		FreakOut(err)
		if domains.Description != "" {
			FreakOut(fmt.Errorf("Error in GET /v2/shared_domains: %s", domains))
		}
		allDomains.Resources = append(allDomains.Resources, domains.Resources...)

		if domains.NextUrl != "" {
			nextUrl = domains.NextUrl
		} else {
			nextUrl = ""
		}
	}
	domainGuid := allDomains.Resources[0].Metadata.Guid
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", "v2/routes", "-X", "POST", "-d", fmt.Sprintf(`{"host":"%s","domain_guid":"%s","space_guid":"%s"}`, fc.Args()[1], domainGuid, space.Guid))
	FreakOut(err)
	output = strings.Join(rawOutput, "")

	var routeGuid string
	if strings.Contains(output, "CF-RouteHostTaken") {
		rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", fmt.Sprintf("v2/routes?q=host:%s;domain_guid:%s", fc.Args()[1], domainGuid))
		FreakOut(err)
		output = strings.Join(rawOutput, "")
		routes := RoutesModel{}
		err = json.Unmarshal([]byte(output), &routes)
		if routes.Description != "" {
			FreakOut(fmt.Errorf("Error in GET /v2/shared_domains: %s", routes))
		}
		routeGuid = routes.Routes[0].Metadata.Guid
	} else {
		route := RouteModel{}
		err = json.Unmarshal([]byte(output), &route)
		if err != nil {
			FreakOut(errors.New("error unmarshaling the route: " + err.Error()))
		}
		if route.Description != "" {
			FreakOut(fmt.Errorf("Error in GET /v2/shared_domains: %s", route))
		}
		routeGuid = route.Metadata.Guid
	}

	//map the route to the app
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", "/v3/route_mappings", "-X", "POST", "-d", fmt.Sprintf(`{"relationships": { "route": { "guid": "%s" }, "app": { "guid": "%s" } } }`, routeGuid, app.Guid))
	FreakOut(err)
	output = strings.Join(rawOutput, "")
	if strings.Contains(output, "errors") {
		FreakOut(fmt.Errorf("/v3/route_mappings failed: %s", output))
	}

	//start the app
	rawOutput, err = cliConnection.CliCommandWithoutTerminalOutput("curl", fmt.Sprintf("/v3/apps/%s/start", app.Guid), "-X", "PUT")
	FreakOut(err)
	output = strings.Join(rawOutput, "")
	if strings.Contains(output, "errors") {
		FreakOut(fmt.Errorf("/v3/apps/%s/start failed: %s", app.Guid, output))
	}

	fmt.Println("Done pushing! Checkout your processes using 'cf apps'")
}
