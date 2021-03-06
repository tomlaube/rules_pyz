package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const pypiRulesHeader = `# AUTO GENERATED. DO NOT EDIT DIRECTLY.
#
# Command line:
#   pypi/pip_generate \
#     %s

load("%s", "%s")
`

var pipLogLinkPattern = regexp.MustCompile(`^\s*(Found|Skipping) link\s*(http[^ #]+\.whl)`)

var platformDefs = []struct {
	bazelPlatform string
	// https://www.python.org/dev/peps/pep-0425/
	pyPIPlatform string
}{
	// not quite right: should include version and "intel" but seems unlikely we will find PPC now
	{"osx", "-cp27-cp27m-macosx_10_"},
	{"linux", "-cp27-cp27mu-manylinux1_x86_64."},
}

// PyPI package names that cannot run correctly inside a zip
var unzipPackages = map[string]bool{
	"certifi": true, // returns paths to the contained .pem files
}

type targetTypeGenerator struct {
	libraryRule    string
	wheelAttribute string
	bzlPath        string
}

var pyzLibraryGenerator = targetTypeGenerator{"pyz_library", "wheels",
	"//rules_python_zip:rules_python_zip.bzl"}
var pexLibraryGenerator = targetTypeGenerator{"pex_library", "eggs",
	"//bazel_rules_pex/pex:pex_rules.bzl"}

// Create a library target.
func (targetGenerator *targetTypeGenerator) printLibTarget(
	dependency *pyPIDependency,
	workspacePrefix *string,
	rulesWorkspace *string,
	installedPackages *map[string]bool) string {
	output := ""

	output += fmt.Sprintf("    %s(\n", targetGenerator.libraryRule)
	output += fmt.Sprintf("        name=\"%s\",\n", dependency.bazelLibraryName())
	if len(dependency.wheels) == 1 {
		output += fmt.Sprintf("        %s=[\"%s\"],\n",
			targetGenerator.wheelAttribute, dependency.wheels[0].bazelTarget(workspacePrefix))
	} else {
		output += fmt.Sprintf("        %s=select({\n", targetGenerator.wheelAttribute)
		for _, wheelInfo := range dependency.wheels {
			selectPlatform := bazelPlatform(wheelInfo.fileName())
			if selectPlatform == "" {
				selectPlatform = "//conditions:default"
			} else {
				selectPlatform = *rulesWorkspace + "//rules_python_zip:" + selectPlatform
			}
			output += fmt.Sprintf("                \"%s\": [\"%s\"],\n",
				selectPlatform, wheelInfo.bazelTarget(workspacePrefix))
		}
		output += fmt.Sprintf("        }),\n")
	}

	if unzipPackages[dependency.name] {
		output += fmt.Sprintf("        zip_safe=False,\n")
	}

	output += fmt.Sprintf("        deps=[\n")
	for _, dep := range dependency.wheels[0].deps {
		output += fmt.Sprintf("            \":%s\",\n", pyPIToBazelPackageName(dep))
	}
	output += fmt.Sprintf("        ],\n")
	// Fixes build error TODO: different type? comment that this is not the right license?
	output += fmt.Sprintf("        licenses=[\"notice\"],\n")
	output += fmt.Sprintf("        visibility=[\"//visibility:public\"],\n")
	output += fmt.Sprintf("    )\n")

	// ensure output is reproducible: output extras in the same order
	extraNames := []string{}
	for extraName := range dependency.wheels[0].extras {
		extraNames = append(extraNames, extraName)
	}
	sort.Strings(extraNames)
	// TODO: Refactor common code out of this and the above?
	for _, extraName := range extraNames {
		extraDeps := dependency.wheels[0].extras[extraName]
		sort.Strings(extraDeps)
		// only include the extra if we have all the referenced packages
		hasAllPackages := true
		for _, dep := range extraDeps {
			if !(*installedPackages)[dependencyNormalizedPackageName(dep)] {
				hasAllPackages = false
				break
			}
		}
		if !hasAllPackages {
			continue
		}

		// Sort dependencies.
		allDeps := append([]string(nil), extraDeps...)
		allDeps = append(allDeps, dependency.bazelLibraryName())
		sort.Strings(allDeps)

		output += fmt.Sprintf("    %s(\n", targetGenerator.libraryRule)
		output += fmt.Sprintf("        name=\"%s__%s\",\n", dependency.bazelLibraryName(), extraName)
		output += fmt.Sprintf("        deps=[\n")
		for _, dep := range allDeps {
			output += fmt.Sprintf("            \":%s\",\n", pyPIToBazelPackageName(dep))
		}
		output += fmt.Sprintf("        ],\n")
		// output += fmt.Sprintf("        # Not the correct license but fixes a build error\n")
		output += fmt.Sprintf("        licenses=[\"notice\"],\n")
		output += fmt.Sprintf("        visibility=[\"//visibility:public\"],\n")
		output += fmt.Sprintf("    )\n")
	}
	return output
}

type wheelInfo struct {
	url           string
	sha256        string
	deps          []string
	extras        map[string][]string
	useLocalWheel bool
	filePath      string // Only set if `useLocalWheel` is `true`
}

func (w *wheelInfo) fileName() string {
	return filepath.Base(w.url)
}
func (w *wheelInfo) bazelPlatform() string {
	return bazelPlatform(w.fileName())
}
func (w *wheelInfo) bazelWorkspaceName(workspacePrefix *string) string {
	fileName := w.fileName()
	packageName := fileName[:strings.IndexByte(fileName, '-')]
	name := *workspacePrefix + pyPIToBazelPackageName(packageName)
	platform := w.bazelPlatform()
	if platform != "" {
		name += "__" + platform
	}
	return name
}
func (w *wheelInfo) makeBazelRule(name *string, wheelDir *string) string {
	output := ""
	if w.useLocalWheel {
		output += fmt.Sprintf("    native.filegroup(\n")
		output += fmt.Sprintf("        name=\"%s\",\n", *name)
		output += fmt.Sprintf("        srcs=[\"%s\"],\n", path.Join(*wheelDir, filepath.Base(w.filePath)))
		// Fixes build error TODO: different type? comment that this is not the right license?
		output += fmt.Sprintf("        licenses=[\"notice\"],\n")
		output += fmt.Sprintf("    )\n")
	} else {
		output += fmt.Sprintf("    if not \"%s\" in native.existing_rules():\n", *name)
		output += fmt.Sprintf("        native.http_file(\n")
		output += fmt.Sprintf("            name=\"%s\",\n", *name)
		output += fmt.Sprintf("            url=\"%s\",\n", w.url)
		output += fmt.Sprintf("            sha256=\"%s\",\n", w.sha256)
		output += fmt.Sprintf("        )\n")
	}
	return output
}
func (w *wheelInfo) wheelFilegroupTarget(workspacePrefix *string) string {
	return fmt.Sprintf(":%s_whl", w.bazelWorkspaceName(workspacePrefix))
}
func (w *wheelInfo) bazelTarget(workspacePrefix *string) string {
	if w.useLocalWheel {
		// We make `filegroup` targets for locally downloaded wheels.
		return fmt.Sprintf(":%s", w.bazelWorkspaceName(workspacePrefix))
	}
	// We use `http_file` repository rules for wheels available directly from PyPI.
	return fmt.Sprintf("@%s//file", w.bazelWorkspaceName(workspacePrefix))

}

type wheelsByPlatform []wheelInfo

func (w wheelsByPlatform) Len() int          { return len(w) }
func (w wheelsByPlatform) Swap(i int, j int) { w[i], w[j] = w[j], w[i] }
func (w wheelsByPlatform) Less(i int, j int) bool {
	// platform then file name to resolve ties
	iPlatform := w[i].bazelPlatform()
	jPlatform := w[j].bazelPlatform()
	if iPlatform < jPlatform {
		return true
	}
	if iPlatform > jPlatform {
		return false
	}

	return w[i].fileName() < w[j].fileName()
}

type pyPIDependency struct {
	name   string
	wheels []wheelInfo
}

func (p *pyPIDependency) bazelLibraryName() string {
	return pyPIToBazelPackageName(p.name)
}

func pyPIToBazelPackageName(packageName string) string {
	// PyPI packages can contain upper case characters, but they are matched insensitively:
	// drop the capitalization for Bazel
	packageName = strings.ToLower(packageName)
	// PyPI packages contain -, but the wheel and bazel names convert them to _
	packageName = strings.Replace(packageName, "-", "_", -1)
	packageName = strings.Replace(packageName, ".", "_", -1)

	// If the package contains an extras suffix of [], replace it with __
	packageName = strings.Replace(packageName, "[", "__", -1)
	packageName = strings.Replace(packageName, "]", "", -1)
	return packageName
}

// Takes a PyPI dependency and returns just the package name part, without extras
func dependencyNormalizedPackageName(dependency string) string {
	// PyPI packages can contain upper case characters, but are matched insensitively
	packageName := strings.ToLower(dependency)
	// PyPI packages contain -, but the wheel and bazel names convert them to _
	packageName = strings.Replace(packageName, "-", "_", -1)

	extraStart := strings.IndexByte(packageName, '[')
	if extraStart >= 0 {
		packageName = packageName[:extraStart]
	}
	return normalizePackageName(packageName)
}

// Returns the wheel package name and version
func wheelFileParts(filename string) (string, string) {
	parts := strings.SplitN(filename, "-", 3)
	return parts[0], parts[1]
}

func sha256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	err2 := f.Close()
	if err != nil {
		return "", err
	}
	if err2 != nil {
		return "", err2
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type wheelToolOutput struct {
	Requires []string            `json:"requires"`
	Extras   map[string][]string `json:"extras"`
}

func wheelDependencies(pythonPath string, wheelToolPath string, path string, verbose bool) ([]string, map[string][]string, error) {
	start := time.Now()
	wheelToolProcess := exec.Command(pythonPath, wheelToolPath, path)
	wheelToolProcess.Stderr = os.Stderr
	outputBytes, err := wheelToolProcess.Output()
	if err != nil {
		fmt.Printf("wheeltool failed on wheel %s; output:\n%s", path, outputBytes)
		return nil, nil, err
	}
	end := time.Now()
	if verbose {
		fmt.Printf("wheeltool %s took %s\n", filepath.Base(path), end.Sub(start).String())
	}
	output := &wheelToolOutput{}
	err = json.Unmarshal(outputBytes, output)
	if err != nil {
		fmt.Printf("Failed to parse wheeltool for wheel %s, output:\n%s", path, output)
		return nil, nil, err
	}
	sort.Strings(output.Requires)
	for _, extraDeps := range output.Extras {
		sort.Strings(extraDeps)
	}
	return output.Requires, output.Extras, nil
}

func bazelPlatform(filename string) string {
	for _, platformDef := range platformDefs {
		if strings.Contains(filename, platformDef.pyPIPlatform) {
			return platformDef.bazelPlatform
		}
	}
	return ""
}

func download(url string, path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("error downloading %s: %s", url, resp.Status)
	}

	_, err = io.Copy(f, resp.Body)
	err2 := resp.Body.Close()
	if err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}
	return f.Close()
}

func normalizePackageName(packageName string) string {
	return strings.ToLower(packageName)
}

func renameIfNotExists(oldPath string, newPath string) error {
	_, err := os.Stat(newPath)
	if err == nil {
		// file exists: do nothing
		return nil
	} else if !os.IsNotExist(err) {
		// stat error
		return err
	}
	// rename the file
	return os.Rename(oldPath, newPath)
}

func main() {
	requirements := flag.String("requirements", "", "path to requirements.txt")
	outputDir := flag.String("outputDir", "", "Base directory where generated files will be placed")
	outputBzlFileName := flag.String("outputBzlFileName", "pypi_rules.bzl", "File name of generated .bzl file (placed in --outputDir)")
	wheelDir := flag.String("wheelDir", "wheels", "Directory to save wheels, relative to --outputDir")
	preferPyPI := flag.Bool("preferPyPI", true, "download from PyPI if possible")
	rulesWorkspace := flag.String("rulesWorkspace", "@rules_pyz",
		"Bazel Workspace path for rules_python_zip")
	ruleType := flag.String("rulesType", "pyz", "Type of rules to generate: pyz or pex")
	verbose := flag.Bool("verbose", false, "Log verbose output; log pip output")
	wheelToolPath := flag.String("wheelToolPath", "./wheeltool.py",
		"Path to tool to output requirements from a wheel")
	pythonPath := flag.String("pythonPath", "python", "Path to version of Python to use when running pip")
	workspacePrefix := flag.String("workspacePrefix", "pypi_", "Prefix for generated repo rules")
	shouldDeleteUnusedWheels := flag.Bool("deleteUnusedWheels", false, "Whether to delete wheels in `wheelDir` that are no longer used")
	flag.Parse()
	if *requirements == "" || *outputDir == "" {
		fmt.Fprintln(os.Stderr, "Error: -requirements and -outputDir are required")
		flag.Usage()
		os.Exit(1)
	}
	if *ruleType != "pyz" && *ruleType != "pex" {
		fmt.Fprintln(os.Stderr, "Error: -ruleType must be pyz or pex")
		os.Exit(1)
	}
	targetGenerator := pyzLibraryGenerator
	if *ruleType == "pex" {
		targetGenerator = pexLibraryGenerator
	}

	fullWheelDir := path.Join(*outputDir, *wheelDir)
	if *wheelDir != "" {
		stat, err := os.Stat(fullWheelDir)
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error: -wheelDir='%s' does not exist\n", fullWheelDir)
			os.Exit(1)
		} else if err != nil {
			panic(err)
		} else if !stat.IsDir() {
			fmt.Fprintf(os.Stderr, "Error: -wheelDir='%s' is not a directory\n", fullWheelDir)
			os.Exit(1)
		}
	}

	rulesBzlPath := *rulesWorkspace + targetGenerator.bzlPath

	output := path.Join(*outputDir, *outputBzlFileName)
	outputBzlFile, err := os.OpenFile(output, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0644)
	if err != nil {
		panic(err)
	}
	defer outputBzlFile.Close()

	tempDir, err := ioutil.TempDir("", "")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tempDir)

	pipProcess := exec.Command(*pythonPath, "-m", "pip", "wheel", "--verbose", "--disable-pip-version-check",
		"--requirement", *requirements, "--wheel-dir", tempDir)
	stdout, err := pipProcess.StdoutPipe()
	if err != nil {
		panic(err)
	}
	pipProcess.Stderr = os.Stderr
	fmt.Println("Running pip to resolve dependencies...")
	if *verbose {
		fmt.Printf("  command: %s %s\n", *pythonPath, strings.Join(pipProcess.Args, " "))
	}
	pipStart := time.Now()
	err = pipProcess.Start()
	if err != nil {
		panic(err)
	}

	wheelFilenameToLink := map[string]string{}
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if *verbose {
			os.Stdout.Write(scanner.Bytes())
			os.Stdout.WriteString("\n")
		}
		matches := pipLogLinkPattern.FindSubmatch(scanner.Bytes())
		if len(matches) > 0 {
			link := matches[2]
			lastSlashIndex := bytes.LastIndexByte(link, '/')
			if lastSlashIndex == -1 {
				panic("invalid link: " + string(link))
			}
			filename := string(link[lastSlashIndex+1:])
			wheelFilenameToLink[filename] = string(link)
		}
	}
	if scanner.Err() != nil {
		panic(scanner.Err())
	}
	err = stdout.Close()
	if err != nil {
		panic(err)
	}
	err = pipProcess.Wait()
	if err != nil {
		panic(err)
	}
	pipEnd := time.Now()
	fmt.Printf("pip executed in %v\n", pipEnd.Sub(pipStart).String())

	fmt.Printf("Processing downloaded wheels...\n")
	dirEntries, err := ioutil.ReadDir(tempDir)
	if err != nil {
		panic(err)
	}
	installedPackages := map[string]bool{}
	dependencies := []pyPIDependency{}
	for _, entry := range dirEntries {
		link := wheelFilenameToLink[entry.Name()]
		hasPyPILink := len(link) > 0
		if !*preferPyPI || !hasPyPILink {
			hasPyPILink = false
			link = entry.Name()
		}

		wheelPath := path.Join(tempDir, entry.Name())
		if *wheelDir != "" && !hasPyPILink {
			// use the existing wheel in wheelDir if it exists; otherwise update it
			// avoids unnecessarily updating dependencies due to possible non-reproducible behaviour
			// in pip or other tools
			destWheelPath := path.Join(fullWheelDir, entry.Name())
			err = renameIfNotExists(wheelPath, destWheelPath)
			if err != nil {
				panic(err)
			}
			wheelPath = path.Join(fullWheelDir, entry.Name())
		}
		// TODO: Refactor this whole mess into another function somewhere
		type wheelFilePartialInfo struct {
			url           string
			filePath      string
			useLocalWheel bool
		}
		wheelFiles := []wheelFilePartialInfo{wheelFilePartialInfo{link, wheelPath, !hasPyPILink}}

		packageName, version := wheelFileParts(entry.Name())

		bazelPlatform := bazelPlatform(entry.Name())
		if bazelPlatform != "" {
			// attempt to find all other platform wheels
			platformToWheelLink := map[string]string{}
			matchPrefix := packageName + "-" + version + "-"
			for wheelFile, link := range wheelFilenameToLink {
				if strings.HasPrefix(wheelFile, matchPrefix) {
					for _, platformDef := range platformDefs {
						if platformDef.bazelPlatform == bazelPlatform {
							continue
						}
						if strings.Contains(wheelFile, platformDef.pyPIPlatform) {
							existingWheelLink := platformToWheelLink[platformDef.bazelPlatform]
							if existingWheelLink != "" {
								// There are two versions. Need to pick one. For
								// now, just pick alphabetically largest to ensure
								// determinism.
								fmt.Fprintf(os.Stderr, "Warning: two acceptable wheels found\n")
								if link < existingWheelLink {
									fmt.Fprintf(os.Stderr, "...picking %s instead of %s\n",
										filepath.Base(existingWheelLink), filepath.Base(link))
									link = existingWheelLink
								} else {
									fmt.Fprintf(os.Stderr, "...picking %s instead of %s\n",
										filepath.Base(link), filepath.Base(existingWheelLink))
								}
							}
							platformToWheelLink[platformDef.bazelPlatform] = link
						}
					}
				}
			}
			if len(platformToWheelLink)+1 != len(platformDefs) {
				fmt.Fprintf(os.Stderr, "Warning: could not find all platformDefs for %s; needs compilation?\n",
					entry.Name())
			}

			// download the other platformDefs and add info for those wheels
			for _, link := range platformToWheelLink {
				// download this PyPI wheel
				filePart := filepath.Base(link)
				destPath := path.Join(tempDir, filePart)
				useLocalWheel := false
				// TODO: Skip download if it already exists; combine with below rename check
				err = download(link, destPath)
				if err != nil {
					panic(err)
				}

				if !*preferPyPI && *wheelDir != "" {
					useLocalWheel = true

					finalPath := path.Join(fullWheelDir, filePart)
					// we do not update the file if it exists, but use finalPath to compute sha256
					err = renameIfNotExists(destPath, finalPath)
					if err != nil {
						panic(err)
					}
					destPath = path.Join(*wheelDir, filePart)
				}
				wheelFiles = append(wheelFiles, wheelFilePartialInfo{link, destPath, useLocalWheel})
			}
		}

		wheels := []wheelInfo{}
		for _, partialInfo := range wheelFiles {
			shaSum, err := sha256Hex(partialInfo.filePath)
			if err != nil {
				panic(err)
			}

			deps, extras, err := wheelDependencies(*pythonPath, *wheelToolPath, partialInfo.filePath, *verbose)
			if err != nil {
				panic(err)
			}

			wheels = append(wheels, wheelInfo{partialInfo.url, shaSum, deps, extras, partialInfo.useLocalWheel, partialInfo.filePath})
		}

		dependencies = append(dependencies, pyPIDependency{packageName, wheels})
		installedPackages[normalizePackageName(packageName)] = true
	}

	commandLineArguments := strings.Join(os.Args[1:], " ")
	fmt.Fprintf(outputBzlFile, pypiRulesHeader, commandLineArguments, rulesBzlPath, targetGenerator.libraryRule)

	// First, make the actual library targets.
	fmt.Fprintf(outputBzlFile, "\ndef pypi_libraries():\n")
	for _, dependency := range dependencies {
		ruleText := targetGenerator.printLibTarget(&dependency, workspacePrefix, rulesWorkspace, &installedPackages)
		fmt.Fprintf(outputBzlFile, ruleText)
	}

	// Next, make `filegroup` targets for any wheels that we stored locally.
	for _, dependency := range dependencies {
		sort.Sort(wheelsByPlatform(dependency.wheels))
		for _, wheel := range dependency.wheels {
			if wheel.useLocalWheel {
				name := wheel.bazelWorkspaceName(workspacePrefix)
				fmt.Fprintf(outputBzlFile, wheel.makeBazelRule(&name, wheelDir))
			}
		}
	}

	// Lastly, make `http_file` repo rules for PyPI links.
	fmt.Fprintln(outputBzlFile, "def pypi_repositories():")
	wroteAtLeastOne := false
	for _, dependency := range dependencies {
		sort.Sort(wheelsByPlatform(dependency.wheels))
		for _, wheel := range dependency.wheels {
			if !wheel.useLocalWheel {
				wroteAtLeastOne = true
				name := wheel.bazelWorkspaceName(workspacePrefix)
				fmt.Fprintf(outputBzlFile, wheel.makeBazelRule(&name, wheelDir))
			}
		}
	}
	if !wroteAtLeastOne {
		fmt.Fprintln(outputBzlFile, "    pass")
	}

	if *shouldDeleteUnusedWheels {
		deleteUnusedWheels(dependencies, path.Join(*outputDir, *wheelDir))
	}

	fmt.Printf("Done\n")
}

func deleteUnusedWheels(dependencies []pyPIDependency, absWheelDir string) {
	// No, go does not have a `set` type. :/
	wheelsThatShouldExist := map[string]bool{}
	for _, dependency := range dependencies {
		for _, wheel := range dependency.wheels {
			wheelsThatShouldExist[wheel.filePath] = true
		}
	}

	wheelPaths, err := filepath.Glob(path.Join(absWheelDir, "*"))
	if err != nil {
		panic(err)
	}
	for _, wheelPath := range wheelPaths {
		_, present := wheelsThatShouldExist[wheelPath]
		if !present {
			fmt.Fprintf(os.Stderr, "Deleting unused wheel: %s\n", wheelPath)
			os.Remove(wheelPath)
		}
	}
}
