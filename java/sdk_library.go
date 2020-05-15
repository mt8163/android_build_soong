// Copyright 2018 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package java

import (
	"fmt"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
)

const (
	sdkXmlFileSuffix    = ".xml"
	permissionsTemplate = `<?xml version=\"1.0\" encoding=\"utf-8\"?>\n` +
		`<!-- Copyright (C) 2018 The Android Open Source Project\n` +
		`\n` +
		`    Licensed under the Apache License, Version 2.0 (the \"License\");\n` +
		`    you may not use this file except in compliance with the License.\n` +
		`    You may obtain a copy of the License at\n` +
		`\n` +
		`        http://www.apache.org/licenses/LICENSE-2.0\n` +
		`\n` +
		`    Unless required by applicable law or agreed to in writing, software\n` +
		`    distributed under the License is distributed on an \"AS IS\" BASIS,\n` +
		`    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.\n` +
		`    See the License for the specific language governing permissions and\n` +
		`    limitations under the License.\n` +
		`-->\n` +
		`<permissions>\n` +
		`    <library name=\"%s\" file=\"%s\"/>\n` +
		`</permissions>\n`
)

// A tag to associated a dependency with a specific api scope.
type scopeDependencyTag struct {
	blueprint.BaseDependencyTag
	name     string
	apiScope *apiScope

	// Function for extracting appropriate path information from the dependency.
	depInfoExtractor func(paths *scopePaths, dep android.Module) error
}

// Extract tag specific information from the dependency.
func (tag scopeDependencyTag) extractDepInfo(ctx android.ModuleContext, dep android.Module, paths *scopePaths) {
	err := tag.depInfoExtractor(paths, dep)
	if err != nil {
		ctx.ModuleErrorf("has an invalid {scopeDependencyTag: %s} dependency on module %s: %s", tag.name, ctx.OtherModuleName(dep), err.Error())
	}
}

// Provides information about an api scope, e.g. public, system, test.
type apiScope struct {
	// The name of the api scope, e.g. public, system, test
	name string

	// The api scope that this scope extends.
	extends *apiScope

	// The legacy enabled status for a specific scope can be dependent on other
	// properties that have been specified on the library so it is provided by
	// a function that can determine the status by examining those properties.
	legacyEnabledStatus func(module *SdkLibrary) bool

	// The default enabled status for non-legacy behavior, which is triggered by
	// explicitly enabling at least one api scope.
	defaultEnabledStatus bool

	// Gets a pointer to the scope specific properties.
	scopeSpecificProperties func(module *SdkLibrary) *ApiScopeProperties

	// The name of the field in the dynamically created structure.
	fieldName string

	// The name of the property in the java_sdk_library_import
	propertyName string

	// The tag to use to depend on the stubs library module.
	stubsTag scopeDependencyTag

	// The tag to use to depend on the stubs source module (if separate from the API module).
	stubsSourceTag scopeDependencyTag

	// The tag to use to depend on the API file generating module (if separate from the stubs source module).
	apiFileTag scopeDependencyTag

	// The tag to use to depend on the stubs source and API module.
	stubsSourceAndApiTag scopeDependencyTag

	// The scope specific prefix to add to the api file base of "current.txt" or "removed.txt".
	apiFilePrefix string

	// The scope specific prefix to add to the sdk library module name to construct a scope specific
	// module name.
	moduleSuffix string

	// SDK version that the stubs library is built against. Note that this is always
	// *current. Older stubs library built with a numbered SDK version is created from
	// the prebuilt jar.
	sdkVersion string

	// Extra arguments to pass to droidstubs for this scope.
	droidstubsArgs []string

	// The args that must be passed to droidstubs to generate the stubs source
	// for this scope.
	//
	// The stubs source must include the definitions of everything that is in this
	// api scope and all the scopes that this one extends.
	droidstubsArgsForGeneratingStubsSource []string

	// The args that must be passed to droidstubs to generate the API for this scope.
	//
	// The API only includes the additional members that this scope adds over the scope
	// that it extends.
	droidstubsArgsForGeneratingApi []string

	// True if the stubs source and api can be created by the same metalava invocation.
	createStubsSourceAndApiTogether bool

	// Whether the api scope can be treated as unstable, and should skip compat checks.
	unstable bool
}

// Initialize a scope, creating and adding appropriate dependency tags
func initApiScope(scope *apiScope) *apiScope {
	name := scope.name
	scope.propertyName = strings.ReplaceAll(name, "-", "_")
	scope.fieldName = proptools.FieldNameForProperty(scope.propertyName)
	scope.stubsTag = scopeDependencyTag{
		name:             name + "-stubs",
		apiScope:         scope,
		depInfoExtractor: (*scopePaths).extractStubsLibraryInfoFromDependency,
	}
	scope.stubsSourceTag = scopeDependencyTag{
		name:             name + "-stubs-source",
		apiScope:         scope,
		depInfoExtractor: (*scopePaths).extractStubsSourceInfoFromDep,
	}
	scope.apiFileTag = scopeDependencyTag{
		name:             name + "-api",
		apiScope:         scope,
		depInfoExtractor: (*scopePaths).extractApiInfoFromDep,
	}
	scope.stubsSourceAndApiTag = scopeDependencyTag{
		name:             name + "-stubs-source-and-api",
		apiScope:         scope,
		depInfoExtractor: (*scopePaths).extractStubsSourceAndApiInfoFromApiStubsProvider,
	}

	// To get the args needed to generate the stubs source append all the args from
	// this scope and all the scopes it extends as each set of args adds additional
	// members to the stubs.
	var stubsSourceArgs []string
	for s := scope; s != nil; s = s.extends {
		stubsSourceArgs = append(stubsSourceArgs, s.droidstubsArgs...)
	}
	scope.droidstubsArgsForGeneratingStubsSource = stubsSourceArgs

	// Currently the args needed to generate the API are the same as the args
	// needed to add additional members.
	apiArgs := scope.droidstubsArgs
	scope.droidstubsArgsForGeneratingApi = apiArgs

	// If the args needed to generate the stubs and API are the same then they
	// can be generated in a single invocation of metalava, otherwise they will
	// need separate invocations.
	scope.createStubsSourceAndApiTogether = reflect.DeepEqual(stubsSourceArgs, apiArgs)

	return scope
}

func (scope *apiScope) stubsLibraryModuleName(baseName string) string {
	return baseName + ".stubs" + scope.moduleSuffix
}

func (scope *apiScope) stubsSourceModuleName(baseName string) string {
	return baseName + ".stubs.source" + scope.moduleSuffix
}

func (scope *apiScope) apiModuleName(baseName string) string {
	return baseName + ".api" + scope.moduleSuffix
}

func (scope *apiScope) String() string {
	return scope.name
}

type apiScopes []*apiScope

func (scopes apiScopes) Strings(accessor func(*apiScope) string) []string {
	var list []string
	for _, scope := range scopes {
		list = append(list, accessor(scope))
	}
	return list
}

var (
	apiScopePublic = initApiScope(&apiScope{
		name: "public",

		// Public scope is enabled by default for both legacy and non-legacy modes.
		legacyEnabledStatus: func(module *SdkLibrary) bool {
			return true
		},
		defaultEnabledStatus: true,

		scopeSpecificProperties: func(module *SdkLibrary) *ApiScopeProperties {
			return &module.sdkLibraryProperties.Public
		},
		sdkVersion: "current",
	})
	apiScopeSystem = initApiScope(&apiScope{
		name:                "system",
		extends:             apiScopePublic,
		legacyEnabledStatus: (*SdkLibrary).generateTestAndSystemScopesByDefault,
		scopeSpecificProperties: func(module *SdkLibrary) *ApiScopeProperties {
			return &module.sdkLibraryProperties.System
		},
		apiFilePrefix:  "system-",
		moduleSuffix:   ".system",
		sdkVersion:     "system_current",
		droidstubsArgs: []string{"-showAnnotation android.annotation.SystemApi\\(client=android.annotation.SystemApi.Client.PRIVILEGED_APPS\\)"},
	})
	apiScopeTest = initApiScope(&apiScope{
		name:                "test",
		extends:             apiScopePublic,
		legacyEnabledStatus: (*SdkLibrary).generateTestAndSystemScopesByDefault,
		scopeSpecificProperties: func(module *SdkLibrary) *ApiScopeProperties {
			return &module.sdkLibraryProperties.Test
		},
		apiFilePrefix:  "test-",
		moduleSuffix:   ".test",
		sdkVersion:     "test_current",
		droidstubsArgs: []string{"-showAnnotation android.annotation.TestApi"},
		unstable:       true,
	})
	apiScopeModuleLib = initApiScope(&apiScope{
		name:    "module-lib",
		extends: apiScopeSystem,
		// Module_lib scope is disabled by default in legacy mode.
		//
		// Enabling this would break existing usages.
		legacyEnabledStatus: func(module *SdkLibrary) bool {
			return false
		},
		scopeSpecificProperties: func(module *SdkLibrary) *ApiScopeProperties {
			return &module.sdkLibraryProperties.Module_lib
		},
		apiFilePrefix: "module-lib-",
		moduleSuffix:  ".module_lib",
		sdkVersion:    "module_current",
		droidstubsArgs: []string{
			"--show-annotation android.annotation.SystemApi\\(client=android.annotation.SystemApi.Client.MODULE_LIBRARIES\\)",
		},
	})
	allApiScopes = apiScopes{
		apiScopePublic,
		apiScopeSystem,
		apiScopeTest,
		apiScopeModuleLib,
	}
)

var (
	javaSdkLibrariesLock sync.Mutex
)

// TODO: these are big features that are currently missing
// 1) disallowing linking to the runtime shared lib
// 2) HTML generation

func init() {
	RegisterSdkLibraryBuildComponents(android.InitRegistrationContext)

	android.RegisterMakeVarsProvider(pctx, func(ctx android.MakeVarsContext) {
		javaSdkLibraries := javaSdkLibraries(ctx.Config())
		sort.Strings(*javaSdkLibraries)
		ctx.Strict("JAVA_SDK_LIBRARIES", strings.Join(*javaSdkLibraries, " "))
	})

	// Register sdk member types.
	android.RegisterSdkMemberType(&sdkLibrarySdkMemberType{
		android.SdkMemberTypeBase{
			PropertyName: "java_sdk_libs",
			SupportsSdk:  true,
		},
	})
}

func RegisterSdkLibraryBuildComponents(ctx android.RegistrationContext) {
	ctx.RegisterModuleType("java_sdk_library", SdkLibraryFactory)
	ctx.RegisterModuleType("java_sdk_library_import", sdkLibraryImportFactory)
}

// Properties associated with each api scope.
type ApiScopeProperties struct {
	// Indicates whether the api surface is generated.
	//
	// If this is set for any scope then all scopes must explicitly specify if they
	// are enabled. This is to prevent new usages from depending on legacy behavior.
	//
	// Otherwise, if this is not set for any scope then the default  behavior is
	// scope specific so please refer to the scope specific property documentation.
	Enabled *bool

	// The sdk_version to use for building the stubs.
	//
	// If not specified then it will use an sdk_version determined as follows:
	// 1) If the sdk_version specified on the java_sdk_library is none then this
	//    will be none. This is used for java_sdk_library instances that are used
	//    to create stubs that contribute to the core_current sdk version.
	// 2) Otherwise, it is assumed that this library extends but does not contribute
	//    directly to a specific sdk_version and so this uses the sdk_version appropriate
	//    for the api scope. e.g. public will use sdk_version: current, system will use
	//    sdk_version: system_current, etc.
	//
	// This does not affect the sdk_version used for either generating the stubs source
	// or the API file. They both have to use the same sdk_version as is used for
	// compiling the implementation library.
	Sdk_version *string
}

type sdkLibraryProperties struct {
	// Visibility for stubs library modules. If not specified then defaults to the
	// visibility property.
	Stubs_library_visibility []string

	// Visibility for stubs source modules. If not specified then defaults to the
	// visibility property.
	Stubs_source_visibility []string

	// List of Java libraries that will be in the classpath when building stubs
	Stub_only_libs []string `android:"arch_variant"`

	// list of package names that will be documented and publicized as API.
	// This allows the API to be restricted to a subset of the source files provided.
	// If this is unspecified then all the source files will be treated as being part
	// of the API.
	Api_packages []string

	// list of package names that must be hidden from the API
	Hidden_api_packages []string

	// the relative path to the directory containing the api specification files.
	// Defaults to "api".
	Api_dir *string

	// If set to true there is no runtime library.
	Api_only *bool

	// local files that are used within user customized droiddoc options.
	Droiddoc_option_files []string

	// additional droiddoc options
	// Available variables for substitution:
	//
	//  $(location <label>): the path to the droiddoc_option_files with name <label>
	Droiddoc_options []string

	// a list of top-level directories containing files to merge qualifier annotations
	// (i.e. those intended to be included in the stubs written) from.
	Merge_annotations_dirs []string

	// a list of top-level directories containing Java stub files to merge show/hide annotations from.
	Merge_inclusion_annotations_dirs []string

	// If set to true, the path of dist files is apistubs/core. Defaults to false.
	Core_lib *bool

	// don't create dist rules.
	No_dist *bool `blueprint:"mutated"`

	// indicates whether system and test apis should be generated.
	Generate_system_and_test_apis bool `blueprint:"mutated"`

	// The properties specific to the public api scope
	//
	// Unless explicitly specified by using public.enabled the public api scope is
	// enabled by default in both legacy and non-legacy mode.
	Public ApiScopeProperties

	// The properties specific to the system api scope
	//
	// In legacy mode the system api scope is enabled by default when sdk_version
	// is set to something other than "none".
	//
	// In non-legacy mode the system api scope is disabled by default.
	System ApiScopeProperties

	// The properties specific to the test api scope
	//
	// In legacy mode the test api scope is enabled by default when sdk_version
	// is set to something other than "none".
	//
	// In non-legacy mode the test api scope is disabled by default.
	Test ApiScopeProperties

	// The properties specific to the module_lib api scope
	//
	// Unless explicitly specified by using test.enabled the module_lib api scope is
	// disabled by default.
	Module_lib ApiScopeProperties

	// Properties related to api linting.
	Api_lint struct {
		// Enable api linting.
		Enabled *bool
	}

	// TODO: determines whether to create HTML doc or not
	//Html_doc *bool
}

type scopePaths struct {
	stubsHeaderPath    android.Paths
	stubsImplPath      android.Paths
	currentApiFilePath android.Path
	removedApiFilePath android.Path
	stubsSrcJar        android.Path
}

func (paths *scopePaths) extractStubsLibraryInfoFromDependency(dep android.Module) error {
	if lib, ok := dep.(Dependency); ok {
		paths.stubsHeaderPath = lib.HeaderJars()
		paths.stubsImplPath = lib.ImplementationJars()
		return nil
	} else {
		return fmt.Errorf("expected module that implements Dependency, e.g. java_library")
	}
}

func (paths *scopePaths) treatDepAsApiStubsProvider(dep android.Module, action func(provider ApiStubsProvider)) error {
	if apiStubsProvider, ok := dep.(ApiStubsProvider); ok {
		action(apiStubsProvider)
		return nil
	} else {
		return fmt.Errorf("expected module that implements ApiStubsProvider, e.g. droidstubs")
	}
}

func (paths *scopePaths) extractApiInfoFromApiStubsProvider(provider ApiStubsProvider) {
	paths.currentApiFilePath = provider.ApiFilePath()
	paths.removedApiFilePath = provider.RemovedApiFilePath()
}

func (paths *scopePaths) extractApiInfoFromDep(dep android.Module) error {
	return paths.treatDepAsApiStubsProvider(dep, func(provider ApiStubsProvider) {
		paths.extractApiInfoFromApiStubsProvider(provider)
	})
}

func (paths *scopePaths) extractStubsSourceInfoFromApiStubsProviders(provider ApiStubsProvider) {
	paths.stubsSrcJar = provider.StubsSrcJar()
}

func (paths *scopePaths) extractStubsSourceInfoFromDep(dep android.Module) error {
	return paths.treatDepAsApiStubsProvider(dep, func(provider ApiStubsProvider) {
		paths.extractStubsSourceInfoFromApiStubsProviders(provider)
	})
}

func (paths *scopePaths) extractStubsSourceAndApiInfoFromApiStubsProvider(dep android.Module) error {
	return paths.treatDepAsApiStubsProvider(dep, func(provider ApiStubsProvider) {
		paths.extractApiInfoFromApiStubsProvider(provider)
		paths.extractStubsSourceInfoFromApiStubsProviders(provider)
	})
}

type commonToSdkLibraryAndImportProperties struct {
	// The naming scheme to use for the components that this module creates.
	//
	// If not specified then it defaults to "default". The other allowable value is
	// "framework-modules" which matches the scheme currently used by framework modules
	// for the equivalent components represented as separate Soong modules.
	//
	// This is a temporary mechanism to simplify conversion from separate modules for each
	// component that follow a different naming pattern to the default one.
	//
	// TODO(b/155480189) - Remove once naming inconsistencies have been resolved.
	Naming_scheme *string
}

// Common code between sdk library and sdk library import
type commonToSdkLibraryAndImport struct {
	moduleBase *android.ModuleBase

	scopePaths map[*apiScope]*scopePaths

	namingScheme sdkLibraryComponentNamingScheme

	commonProperties commonToSdkLibraryAndImportProperties
}

func (c *commonToSdkLibraryAndImport) initCommon(moduleBase *android.ModuleBase) {
	c.moduleBase = moduleBase

	moduleBase.AddProperties(&c.commonProperties)
}

func (c *commonToSdkLibraryAndImport) initCommonAfterDefaultsApplied(ctx android.DefaultableHookContext) bool {
	schemeProperty := proptools.StringDefault(c.commonProperties.Naming_scheme, "default")
	switch schemeProperty {
	case "default":
		c.namingScheme = &defaultNamingScheme{}
	case "framework-modules":
		c.namingScheme = &frameworkModulesNamingScheme{}
	default:
		ctx.PropertyErrorf("naming_scheme", "expected 'default' but was %q", schemeProperty)
		return false
	}

	return true
}

// Name of the java_library module that compiles the stubs source.
func (c *commonToSdkLibraryAndImport) stubsLibraryModuleName(apiScope *apiScope) string {
	return c.namingScheme.stubsLibraryModuleName(apiScope, c.moduleBase.BaseModuleName())
}

// Name of the droidstubs module that generates the stubs source and may also
// generate/check the API.
func (c *commonToSdkLibraryAndImport) stubsSourceModuleName(apiScope *apiScope) string {
	return c.namingScheme.stubsSourceModuleName(apiScope, c.moduleBase.BaseModuleName())
}

// Name of the droidstubs module that generates/checks the API. Only used if it
// requires different arts to the stubs source generating module.
func (c *commonToSdkLibraryAndImport) apiModuleName(apiScope *apiScope) string {
	return c.namingScheme.apiModuleName(apiScope, c.moduleBase.BaseModuleName())
}

func (c *commonToSdkLibraryAndImport) getScopePaths(scope *apiScope) *scopePaths {
	if c.scopePaths == nil {
		c.scopePaths = make(map[*apiScope]*scopePaths)
	}
	paths := c.scopePaths[scope]
	if paths == nil {
		paths = &scopePaths{}
		c.scopePaths[scope] = paths
	}

	return paths
}

type SdkLibrary struct {
	Library

	sdkLibraryProperties sdkLibraryProperties

	// Map from api scope to the scope specific property structure.
	scopeToProperties map[*apiScope]*ApiScopeProperties

	commonToSdkLibraryAndImport
}

var _ Dependency = (*SdkLibrary)(nil)
var _ SdkLibraryDependency = (*SdkLibrary)(nil)

func (module *SdkLibrary) generateTestAndSystemScopesByDefault() bool {
	return module.sdkLibraryProperties.Generate_system_and_test_apis
}

func (module *SdkLibrary) getGeneratedApiScopes(ctx android.EarlyModuleContext) apiScopes {
	// Check to see if any scopes have been explicitly enabled. If any have then all
	// must be.
	anyScopesExplicitlyEnabled := false
	for _, scope := range allApiScopes {
		scopeProperties := module.scopeToProperties[scope]
		if scopeProperties.Enabled != nil {
			anyScopesExplicitlyEnabled = true
			break
		}
	}

	var generatedScopes apiScopes
	enabledScopes := make(map[*apiScope]struct{})
	for _, scope := range allApiScopes {
		scopeProperties := module.scopeToProperties[scope]
		// If any scopes are explicitly enabled then ignore the legacy enabled status.
		// This is to ensure that any new usages of this module type do not rely on legacy
		// behaviour.
		defaultEnabledStatus := false
		if anyScopesExplicitlyEnabled {
			defaultEnabledStatus = scope.defaultEnabledStatus
		} else {
			defaultEnabledStatus = scope.legacyEnabledStatus(module)
		}
		enabled := proptools.BoolDefault(scopeProperties.Enabled, defaultEnabledStatus)
		if enabled {
			enabledScopes[scope] = struct{}{}
			generatedScopes = append(generatedScopes, scope)
		}
	}

	// Now check to make sure that any scope that is extended by an enabled scope is also
	// enabled.
	for _, scope := range allApiScopes {
		if _, ok := enabledScopes[scope]; ok {
			extends := scope.extends
			if extends != nil {
				if _, ok := enabledScopes[extends]; !ok {
					ctx.ModuleErrorf("enabled api scope %q depends on disabled scope %q", scope, extends)
				}
			}
		}
	}

	return generatedScopes
}

var xmlPermissionsFileTag = dependencyTag{name: "xml-permissions-file"}

func IsXmlPermissionsFileDepTag(depTag blueprint.DependencyTag) bool {
	if dt, ok := depTag.(dependencyTag); ok {
		return dt == xmlPermissionsFileTag
	}
	return false
}

func (module *SdkLibrary) DepsMutator(ctx android.BottomUpMutatorContext) {
	for _, apiScope := range module.getGeneratedApiScopes(ctx) {
		// Add dependencies to the stubs library
		ctx.AddVariationDependencies(nil, apiScope.stubsTag, module.stubsLibraryModuleName(apiScope))

		// If the stubs source and API cannot be generated together then add an additional dependency on
		// the API module.
		if apiScope.createStubsSourceAndApiTogether {
			// Add a dependency on the stubs source in order to access both stubs source and api information.
			ctx.AddVariationDependencies(nil, apiScope.stubsSourceAndApiTag, module.stubsSourceModuleName(apiScope))
		} else {
			// Add separate dependencies on the creators of the stubs source files and the API.
			ctx.AddVariationDependencies(nil, apiScope.stubsSourceTag, module.stubsSourceModuleName(apiScope))
			ctx.AddVariationDependencies(nil, apiScope.apiFileTag, module.apiModuleName(apiScope))
		}
	}

	if !proptools.Bool(module.sdkLibraryProperties.Api_only) {
		// Add dependency to the rule for generating the xml permissions file
		ctx.AddDependency(module, xmlPermissionsFileTag, module.xmlFileName())
	}

	module.Library.deps(ctx)
}

func (module *SdkLibrary) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	// Don't build an implementation library if this is api only.
	if !proptools.Bool(module.sdkLibraryProperties.Api_only) {
		module.Library.GenerateAndroidBuildActions(ctx)
	}

	// Record the paths to the header jars of the library (stubs and impl).
	// When this java_sdk_library is depended upon from others via "libs" property,
	// the recorded paths will be returned depending on the link type of the caller.
	ctx.VisitDirectDeps(func(to android.Module) {
		tag := ctx.OtherModuleDependencyTag(to)

		// Extract information from any of the scope specific dependencies.
		if scopeTag, ok := tag.(scopeDependencyTag); ok {
			apiScope := scopeTag.apiScope
			scopePaths := module.getScopePaths(apiScope)

			// Extract information from the dependency. The exact information extracted
			// is determined by the nature of the dependency which is determined by the tag.
			scopeTag.extractDepInfo(ctx, to, scopePaths)
		}
	})
}

func (module *SdkLibrary) AndroidMkEntries() []android.AndroidMkEntries {
	if proptools.Bool(module.sdkLibraryProperties.Api_only) {
		return nil
	}
	entriesList := module.Library.AndroidMkEntries()
	entries := &entriesList[0]
	entries.Required = append(entries.Required, module.xmlFileName())
	return entriesList
}

// Module name of the runtime implementation library
func (module *SdkLibrary) implName() string {
	return module.BaseModuleName()
}

// Module name of the XML file for the lib
func (module *SdkLibrary) xmlFileName() string {
	return module.BaseModuleName() + sdkXmlFileSuffix
}

// The dist path of the stub artifacts
func (module *SdkLibrary) apiDistPath(apiScope *apiScope) string {
	if module.ModuleBase.Owner() != "" {
		return path.Join("apistubs", module.ModuleBase.Owner(), apiScope.name)
	} else if Bool(module.sdkLibraryProperties.Core_lib) {
		return path.Join("apistubs", "core", apiScope.name)
	} else {
		return path.Join("apistubs", "android", apiScope.name)
	}
}

// Get the sdk version for use when compiling the stubs library.
func (module *SdkLibrary) sdkVersionForStubsLibrary(mctx android.EarlyModuleContext, apiScope *apiScope) string {
	scopeProperties := module.scopeToProperties[apiScope]
	if scopeProperties.Sdk_version != nil {
		return proptools.String(scopeProperties.Sdk_version)
	}

	sdkDep := decodeSdkDep(mctx, sdkContext(&module.Library))
	if sdkDep.hasStandardLibs() {
		// If building against a standard sdk then use the sdk version appropriate for the scope.
		return apiScope.sdkVersion
	} else {
		// Otherwise, use no system module.
		return "none"
	}
}

func (module *SdkLibrary) latestApiFilegroupName(apiScope *apiScope) string {
	return ":" + module.BaseModuleName() + ".api." + apiScope.name + ".latest"
}

func (module *SdkLibrary) latestRemovedApiFilegroupName(apiScope *apiScope) string {
	return ":" + module.BaseModuleName() + "-removed.api." + apiScope.name + ".latest"
}

// Creates a static java library that has API stubs
func (module *SdkLibrary) createStubsLibrary(mctx android.DefaultableHookContext, apiScope *apiScope) {
	props := struct {
		Name                *string
		Visibility          []string
		Srcs                []string
		Installable         *bool
		Sdk_version         *string
		System_modules      *string
		Patch_module        *string
		Libs                []string
		Soc_specific        *bool
		Device_specific     *bool
		Product_specific    *bool
		System_ext_specific *bool
		Compile_dex         *bool
		Java_version        *string
		Product_variables   struct {
			Pdk struct {
				Enabled *bool
			}
		}
		Openjdk9 struct {
			Srcs       []string
			Javacflags []string
		}
		Dist struct {
			Targets []string
			Dest    *string
			Dir     *string
			Tag     *string
		}
	}{}

	props.Name = proptools.StringPtr(module.stubsLibraryModuleName(apiScope))

	// If stubs_library_visibility is not set then the created module will use the
	// visibility of this module.
	visibility := module.sdkLibraryProperties.Stubs_library_visibility
	props.Visibility = visibility

	// sources are generated from the droiddoc
	props.Srcs = []string{":" + module.stubsSourceModuleName(apiScope)}
	sdkVersion := module.sdkVersionForStubsLibrary(mctx, apiScope)
	props.Sdk_version = proptools.StringPtr(sdkVersion)
	props.System_modules = module.Library.Module.deviceProperties.System_modules
	props.Patch_module = module.Library.Module.properties.Patch_module
	props.Installable = proptools.BoolPtr(false)
	props.Libs = module.sdkLibraryProperties.Stub_only_libs
	props.Product_variables.Pdk.Enabled = proptools.BoolPtr(false)
	props.Openjdk9.Srcs = module.Library.Module.properties.Openjdk9.Srcs
	props.Openjdk9.Javacflags = module.Library.Module.properties.Openjdk9.Javacflags
	props.Java_version = module.Library.Module.properties.Java_version
	if module.Library.Module.deviceProperties.Compile_dex != nil {
		props.Compile_dex = module.Library.Module.deviceProperties.Compile_dex
	}

	if module.SocSpecific() {
		props.Soc_specific = proptools.BoolPtr(true)
	} else if module.DeviceSpecific() {
		props.Device_specific = proptools.BoolPtr(true)
	} else if module.ProductSpecific() {
		props.Product_specific = proptools.BoolPtr(true)
	} else if module.SystemExtSpecific() {
		props.System_ext_specific = proptools.BoolPtr(true)
	}
	// Dist the class jar artifact for sdk builds.
	if !Bool(module.sdkLibraryProperties.No_dist) {
		props.Dist.Targets = []string{"sdk", "win_sdk"}
		props.Dist.Dest = proptools.StringPtr(fmt.Sprintf("%v.jar", module.BaseModuleName()))
		props.Dist.Dir = proptools.StringPtr(module.apiDistPath(apiScope))
		props.Dist.Tag = proptools.StringPtr(".jar")
	}

	mctx.CreateModule(LibraryFactory, &props)
}

// Creates a droidstubs module that creates stubs source files from the given full source
// files and also updates and checks the API specification files.
func (module *SdkLibrary) createStubsSourcesAndApi(mctx android.DefaultableHookContext, apiScope *apiScope, name string, createStubSources, createApi bool, scopeSpecificDroidstubsArgs []string) {
	props := struct {
		Name                             *string
		Visibility                       []string
		Srcs                             []string
		Installable                      *bool
		Sdk_version                      *string
		System_modules                   *string
		Libs                             []string
		Arg_files                        []string
		Args                             *string
		Java_version                     *string
		Merge_annotations_dirs           []string
		Merge_inclusion_annotations_dirs []string
		Generate_stubs                   *bool
		Check_api                        struct {
			Current                   ApiToCheck
			Last_released             ApiToCheck
			Ignore_missing_latest_api *bool

			Api_lint struct {
				Enabled       *bool
				New_since     *string
				Baseline_file *string
			}
		}
		Aidl struct {
			Include_dirs       []string
			Local_include_dirs []string
		}
		Dist struct {
			Targets []string
			Dest    *string
			Dir     *string
		}
	}{}

	// The stubs source processing uses the same compile time classpath when extracting the
	// API from the implementation library as it does when compiling it. i.e. the same
	// * sdk version
	// * system_modules
	// * libs (static_libs/libs)

	props.Name = proptools.StringPtr(name)

	// If stubs_source_visibility is not set then the created module will use the
	// visibility of this module.
	visibility := module.sdkLibraryProperties.Stubs_source_visibility
	props.Visibility = visibility

	props.Srcs = append(props.Srcs, module.Library.Module.properties.Srcs...)
	props.Sdk_version = module.Library.Module.deviceProperties.Sdk_version
	props.System_modules = module.Library.Module.deviceProperties.System_modules
	props.Installable = proptools.BoolPtr(false)
	// A droiddoc module has only one Libs property and doesn't distinguish between
	// shared libs and static libs. So we need to add both of these libs to Libs property.
	props.Libs = module.Library.Module.properties.Libs
	props.Libs = append(props.Libs, module.Library.Module.properties.Static_libs...)
	props.Aidl.Include_dirs = module.Library.Module.deviceProperties.Aidl.Include_dirs
	props.Aidl.Local_include_dirs = module.Library.Module.deviceProperties.Aidl.Local_include_dirs
	props.Java_version = module.Library.Module.properties.Java_version

	props.Merge_annotations_dirs = module.sdkLibraryProperties.Merge_annotations_dirs
	props.Merge_inclusion_annotations_dirs = module.sdkLibraryProperties.Merge_inclusion_annotations_dirs

	droidstubsArgs := []string{}
	if len(module.sdkLibraryProperties.Api_packages) != 0 {
		droidstubsArgs = append(droidstubsArgs, "--stub-packages "+strings.Join(module.sdkLibraryProperties.Api_packages, ":"))
	}
	if len(module.sdkLibraryProperties.Hidden_api_packages) != 0 {
		droidstubsArgs = append(droidstubsArgs,
			android.JoinWithPrefix(module.sdkLibraryProperties.Hidden_api_packages, " --hide-package "))
	}
	droidstubsArgs = append(droidstubsArgs, module.sdkLibraryProperties.Droiddoc_options...)
	disabledWarnings := []string{
		"MissingPermission",
		"BroadcastBehavior",
		"HiddenSuperclass",
		"DeprecationMismatch",
		"UnavailableSymbol",
		"SdkConstant",
		"HiddenTypeParameter",
		"Todo",
		"Typo",
	}
	droidstubsArgs = append(droidstubsArgs, android.JoinWithPrefix(disabledWarnings, "--hide "))

	if !createStubSources {
		// Stubs are not required.
		props.Generate_stubs = proptools.BoolPtr(false)
	}

	// Add in scope specific arguments.
	droidstubsArgs = append(droidstubsArgs, scopeSpecificDroidstubsArgs...)
	props.Arg_files = module.sdkLibraryProperties.Droiddoc_option_files
	props.Args = proptools.StringPtr(strings.Join(droidstubsArgs, " "))

	if createApi {
		// List of APIs identified from the provided source files are created. They are later
		// compared against to the not-yet-released (a.k.a current) list of APIs and to the
		// last-released (a.k.a numbered) list of API.
		currentApiFileName := apiScope.apiFilePrefix + "current.txt"
		removedApiFileName := apiScope.apiFilePrefix + "removed.txt"
		apiDir := module.getApiDir()
		currentApiFileName = path.Join(apiDir, currentApiFileName)
		removedApiFileName = path.Join(apiDir, removedApiFileName)

		// check against the not-yet-release API
		props.Check_api.Current.Api_file = proptools.StringPtr(currentApiFileName)
		props.Check_api.Current.Removed_api_file = proptools.StringPtr(removedApiFileName)

		if !apiScope.unstable {
			// check against the latest released API
			latestApiFilegroupName := proptools.StringPtr(module.latestApiFilegroupName(apiScope))
			props.Check_api.Last_released.Api_file = latestApiFilegroupName
			props.Check_api.Last_released.Removed_api_file = proptools.StringPtr(
				module.latestRemovedApiFilegroupName(apiScope))
			props.Check_api.Ignore_missing_latest_api = proptools.BoolPtr(true)

			if proptools.Bool(module.sdkLibraryProperties.Api_lint.Enabled) {
				// Enable api lint.
				props.Check_api.Api_lint.Enabled = proptools.BoolPtr(true)
				props.Check_api.Api_lint.New_since = latestApiFilegroupName

				// If it exists then pass a lint-baseline.txt through to droidstubs.
				baselinePath := path.Join(apiDir, apiScope.apiFilePrefix+"lint-baseline.txt")
				baselinePathRelativeToRoot := path.Join(mctx.ModuleDir(), baselinePath)
				paths, err := mctx.GlobWithDeps(baselinePathRelativeToRoot, nil)
				if err != nil {
					mctx.ModuleErrorf("error checking for presence of %s: %s", baselinePathRelativeToRoot, err)
				}
				if len(paths) == 1 {
					props.Check_api.Api_lint.Baseline_file = proptools.StringPtr(baselinePath)
				} else if len(paths) != 0 {
					mctx.ModuleErrorf("error checking for presence of %s: expected one path, found: %v", baselinePathRelativeToRoot, paths)
				}
			}
		}

		// Dist the api txt artifact for sdk builds.
		if !Bool(module.sdkLibraryProperties.No_dist) {
			props.Dist.Targets = []string{"sdk", "win_sdk"}
			props.Dist.Dest = proptools.StringPtr(fmt.Sprintf("%v.txt", module.BaseModuleName()))
			props.Dist.Dir = proptools.StringPtr(path.Join(module.apiDistPath(apiScope), "api"))
		}
	}

	mctx.CreateModule(DroidstubsFactory, &props)
}

func (module *SdkLibrary) DepIsInSameApex(mctx android.BaseModuleContext, dep android.Module) bool {
	depTag := mctx.OtherModuleDependencyTag(dep)
	if depTag == xmlPermissionsFileTag {
		return true
	}
	return module.Library.DepIsInSameApex(mctx, dep)
}

// Creates the xml file that publicizes the runtime library
func (module *SdkLibrary) createXmlFile(mctx android.DefaultableHookContext) {
	props := struct {
		Name                *string
		Lib_name            *string
		Soc_specific        *bool
		Device_specific     *bool
		Product_specific    *bool
		System_ext_specific *bool
		Apex_available      []string
	}{
		Name:           proptools.StringPtr(module.xmlFileName()),
		Lib_name:       proptools.StringPtr(module.BaseModuleName()),
		Apex_available: module.ApexProperties.Apex_available,
	}

	if module.SocSpecific() {
		props.Soc_specific = proptools.BoolPtr(true)
	} else if module.DeviceSpecific() {
		props.Device_specific = proptools.BoolPtr(true)
	} else if module.ProductSpecific() {
		props.Product_specific = proptools.BoolPtr(true)
	} else if module.SystemExtSpecific() {
		props.System_ext_specific = proptools.BoolPtr(true)
	}

	mctx.CreateModule(sdkLibraryXmlFactory, &props)
}

func PrebuiltJars(ctx android.BaseModuleContext, baseName string, s sdkSpec) android.Paths {
	var ver sdkVersion
	var kind sdkKind
	if s.usePrebuilt(ctx) {
		ver = s.version
		kind = s.kind
	} else {
		// We don't have prebuilt SDK for the specific sdkVersion.
		// Instead of breaking the build, fallback to use "system_current"
		ver = sdkVersionCurrent
		kind = sdkSystem
	}

	dir := filepath.Join("prebuilts", "sdk", ver.String(), kind.String())
	jar := filepath.Join(dir, baseName+".jar")
	jarPath := android.ExistentPathForSource(ctx, jar)
	if !jarPath.Valid() {
		if ctx.Config().AllowMissingDependencies() {
			return android.Paths{android.PathForSource(ctx, jar)}
		} else {
			ctx.PropertyErrorf("sdk_library", "invalid sdk version %q, %q does not exist", s.raw, jar)
		}
		return nil
	}
	return android.Paths{jarPath.Path()}
}

func (module *SdkLibrary) sdkJars(
	ctx android.BaseModuleContext,
	sdkVersion sdkSpec,
	headerJars bool) android.Paths {

	// If a specific numeric version has been requested then use prebuilt versions of the sdk.
	if sdkVersion.version.isNumbered() {
		return PrebuiltJars(ctx, module.BaseModuleName(), sdkVersion)
	} else {
		if !sdkVersion.specified() {
			if headerJars {
				return module.Library.HeaderJars()
			} else {
				return module.Library.ImplementationJars()
			}
		}
		var apiScope *apiScope
		switch sdkVersion.kind {
		case sdkSystem:
			apiScope = apiScopeSystem
		case sdkTest:
			apiScope = apiScopeTest
		case sdkPrivate:
			return module.Library.HeaderJars()
		default:
			apiScope = apiScopePublic
		}

		paths := module.getScopePaths(apiScope)
		if headerJars {
			return paths.stubsHeaderPath
		} else {
			return paths.stubsImplPath
		}
	}
}

// to satisfy SdkLibraryDependency interface
func (module *SdkLibrary) SdkHeaderJars(ctx android.BaseModuleContext, sdkVersion sdkSpec) android.Paths {
	return module.sdkJars(ctx, sdkVersion, true /*headerJars*/)
}

// to satisfy SdkLibraryDependency interface
func (module *SdkLibrary) SdkImplementationJars(ctx android.BaseModuleContext, sdkVersion sdkSpec) android.Paths {
	return module.sdkJars(ctx, sdkVersion, false /*headerJars*/)
}

func (module *SdkLibrary) SetNoDist() {
	module.sdkLibraryProperties.No_dist = proptools.BoolPtr(true)
}

var javaSdkLibrariesKey = android.NewOnceKey("javaSdkLibraries")

func javaSdkLibraries(config android.Config) *[]string {
	return config.Once(javaSdkLibrariesKey, func() interface{} {
		return &[]string{}
	}).(*[]string)
}

func (module *SdkLibrary) getApiDir() string {
	return proptools.StringDefault(module.sdkLibraryProperties.Api_dir, "api")
}

// For a java_sdk_library module, create internal modules for stubs, docs,
// runtime libs and xml file. If requested, the stubs and docs are created twice
// once for public API level and once for system API level
func (module *SdkLibrary) CreateInternalModules(mctx android.DefaultableHookContext) {
	// If the module has been disabled then don't create any child modules.
	if !module.Enabled() {
		return
	}

	if len(module.Library.Module.properties.Srcs) == 0 {
		mctx.PropertyErrorf("srcs", "java_sdk_library must specify srcs")
		return
	}

	// If this builds against standard libraries (i.e. is not part of the core libraries)
	// then assume it provides both system and test apis. Otherwise, assume it does not and
	// also assume it does not contribute to the dist build.
	sdkDep := decodeSdkDep(mctx, sdkContext(&module.Library))
	hasSystemAndTestApis := sdkDep.hasStandardLibs()
	module.sdkLibraryProperties.Generate_system_and_test_apis = hasSystemAndTestApis
	module.sdkLibraryProperties.No_dist = proptools.BoolPtr(!hasSystemAndTestApis)

	missing_current_api := false

	generatedScopes := module.getGeneratedApiScopes(mctx)

	apiDir := module.getApiDir()
	for _, scope := range generatedScopes {
		for _, api := range []string{"current.txt", "removed.txt"} {
			path := path.Join(mctx.ModuleDir(), apiDir, scope.apiFilePrefix+api)
			p := android.ExistentPathForSource(mctx, path)
			if !p.Valid() {
				mctx.ModuleErrorf("Current api file %#v doesn't exist", path)
				missing_current_api = true
			}
		}
	}

	if missing_current_api {
		script := "build/soong/scripts/gen-java-current-api-files.sh"
		p := android.ExistentPathForSource(mctx, script)

		if !p.Valid() {
			panic(fmt.Sprintf("script file %s doesn't exist", script))
		}

		mctx.ModuleErrorf("One or more current api files are missing. "+
			"You can update them by:\n"+
			"%s %q %s && m update-api",
			script, filepath.Join(mctx.ModuleDir(), apiDir),
			strings.Join(generatedScopes.Strings(func(s *apiScope) string { return s.apiFilePrefix }), " "))
		return
	}

	for _, scope := range generatedScopes {
		stubsSourceArgs := scope.droidstubsArgsForGeneratingStubsSource
		stubsSourceModuleName := module.stubsSourceModuleName(scope)

		// If the args needed to generate the stubs and API are the same then they
		// can be generated in a single invocation of metalava, otherwise they will
		// need separate invocations.
		if scope.createStubsSourceAndApiTogether {
			// Use the stubs source name for legacy reasons.
			module.createStubsSourcesAndApi(mctx, scope, stubsSourceModuleName, true, true, stubsSourceArgs)
		} else {
			module.createStubsSourcesAndApi(mctx, scope, stubsSourceModuleName, true, false, stubsSourceArgs)

			apiArgs := scope.droidstubsArgsForGeneratingApi
			apiName := module.apiModuleName(scope)
			module.createStubsSourcesAndApi(mctx, scope, apiName, false, true, apiArgs)
		}

		module.createStubsLibrary(mctx, scope)
	}

	if !proptools.Bool(module.sdkLibraryProperties.Api_only) {
		// for runtime
		module.createXmlFile(mctx)

		// record java_sdk_library modules so that they are exported to make
		javaSdkLibraries := javaSdkLibraries(mctx.Config())
		javaSdkLibrariesLock.Lock()
		defer javaSdkLibrariesLock.Unlock()
		*javaSdkLibraries = append(*javaSdkLibraries, module.BaseModuleName())
	}
}

func (module *SdkLibrary) InitSdkLibraryProperties() {
	module.AddProperties(
		&module.sdkLibraryProperties,
		&module.Library.Module.properties,
		&module.Library.Module.dexpreoptProperties,
		&module.Library.Module.deviceProperties,
		&module.Library.Module.protoProperties,
	)

	module.Library.Module.properties.Installable = proptools.BoolPtr(true)
	module.Library.Module.deviceProperties.IsSDKLibrary = true
}

// Defines how to name the individual component modules the sdk library creates.
type sdkLibraryComponentNamingScheme interface {
	stubsLibraryModuleName(scope *apiScope, baseName string) string

	stubsSourceModuleName(scope *apiScope, baseName string) string

	apiModuleName(scope *apiScope, baseName string) string
}

type defaultNamingScheme struct {
}

func (s *defaultNamingScheme) stubsLibraryModuleName(scope *apiScope, baseName string) string {
	return scope.stubsLibraryModuleName(baseName)
}

func (s *defaultNamingScheme) stubsSourceModuleName(scope *apiScope, baseName string) string {
	return scope.stubsSourceModuleName(baseName)
}

func (s *defaultNamingScheme) apiModuleName(scope *apiScope, baseName string) string {
	return scope.apiModuleName(baseName)
}

var _ sdkLibraryComponentNamingScheme = (*defaultNamingScheme)(nil)

type frameworkModulesNamingScheme struct {
}

func (s *frameworkModulesNamingScheme) moduleSuffix(scope *apiScope) string {
	suffix := scope.name
	if scope == apiScopeModuleLib {
		suffix = "module_libs_"
	}
	return suffix
}

func (s *frameworkModulesNamingScheme) stubsLibraryModuleName(scope *apiScope, baseName string) string {
	return fmt.Sprintf("%s-stubs-%sapi", baseName, s.moduleSuffix(scope))
}

func (s *frameworkModulesNamingScheme) stubsSourceModuleName(scope *apiScope, baseName string) string {
	return fmt.Sprintf("%s-stubs-srcs-%sapi", baseName, s.moduleSuffix(scope))
}

func (s *frameworkModulesNamingScheme) apiModuleName(scope *apiScope, baseName string) string {
	return fmt.Sprintf("%s-api-%sapi", baseName, s.moduleSuffix(scope))
}

var _ sdkLibraryComponentNamingScheme = (*frameworkModulesNamingScheme)(nil)

// java_sdk_library is a special Java library that provides optional platform APIs to apps.
// In practice, it can be viewed as a combination of several modules: 1) stubs library that clients
// are linked against to, 2) droiddoc module that internally generates API stubs source files,
// 3) the real runtime shared library that implements the APIs, and 4) XML file for adding
// the runtime lib to the classpath at runtime if requested via <uses-library>.
func SdkLibraryFactory() android.Module {
	module := &SdkLibrary{}

	// Initialize information common between source and prebuilt.
	module.initCommon(&module.ModuleBase)

	module.InitSdkLibraryProperties()
	android.InitApexModule(module)
	InitJavaModule(module, android.HostAndDeviceSupported)

	// Initialize the map from scope to scope specific properties.
	scopeToProperties := make(map[*apiScope]*ApiScopeProperties)
	for _, scope := range allApiScopes {
		scopeToProperties[scope] = scope.scopeSpecificProperties(module)
	}
	module.scopeToProperties = scopeToProperties

	// Add the properties containing visibility rules so that they are checked.
	android.AddVisibilityProperty(module, "stubs_library_visibility", &module.sdkLibraryProperties.Stubs_library_visibility)
	android.AddVisibilityProperty(module, "stubs_source_visibility", &module.sdkLibraryProperties.Stubs_source_visibility)

	module.SetDefaultableHook(func(ctx android.DefaultableHookContext) {
		if module.initCommonAfterDefaultsApplied(ctx) {
			module.CreateInternalModules(ctx)
		}
	})
	return module
}

//
// SDK library prebuilts
//

// Properties associated with each api scope.
type sdkLibraryScopeProperties struct {
	Jars []string `android:"path"`

	Sdk_version *string

	// List of shared java libs that this module has dependencies to
	Libs []string

	// The stubs source.
	Stub_srcs []string `android:"path"`

	// The current.txt
	Current_api string `android:"path"`

	// The removed.txt
	Removed_api string `android:"path"`
}

type sdkLibraryImportProperties struct {
	// List of shared java libs, common to all scopes, that this module has
	// dependencies to
	Libs []string
}

type sdkLibraryImport struct {
	android.ModuleBase
	android.DefaultableModuleBase
	prebuilt android.Prebuilt
	android.ApexModuleBase
	android.SdkBase

	properties sdkLibraryImportProperties

	// Map from api scope to the scope specific property structure.
	scopeProperties map[*apiScope]*sdkLibraryScopeProperties

	commonToSdkLibraryAndImport
}

var _ SdkLibraryDependency = (*sdkLibraryImport)(nil)

// The type of a structure that contains a field of type sdkLibraryScopeProperties
// for each apiscope in allApiScopes, e.g. something like:
// struct {
//   Public sdkLibraryScopeProperties
//   System sdkLibraryScopeProperties
//   ...
// }
var allScopeStructType = createAllScopePropertiesStructType()

// Dynamically create a structure type for each apiscope in allApiScopes.
func createAllScopePropertiesStructType() reflect.Type {
	var fields []reflect.StructField
	for _, apiScope := range allApiScopes {
		field := reflect.StructField{
			Name: apiScope.fieldName,
			Type: reflect.TypeOf(sdkLibraryScopeProperties{}),
		}
		fields = append(fields, field)
	}

	return reflect.StructOf(fields)
}

// Create an instance of the scope specific structure type and return a map
// from apiscope to a pointer to each scope specific field.
func createPropertiesInstance() (interface{}, map[*apiScope]*sdkLibraryScopeProperties) {
	allScopePropertiesPtr := reflect.New(allScopeStructType)
	allScopePropertiesStruct := allScopePropertiesPtr.Elem()
	scopeProperties := make(map[*apiScope]*sdkLibraryScopeProperties)

	for _, apiScope := range allApiScopes {
		field := allScopePropertiesStruct.FieldByName(apiScope.fieldName)
		scopeProperties[apiScope] = field.Addr().Interface().(*sdkLibraryScopeProperties)
	}

	return allScopePropertiesPtr.Interface(), scopeProperties
}

// java_sdk_library_import imports a prebuilt java_sdk_library.
func sdkLibraryImportFactory() android.Module {
	module := &sdkLibraryImport{}

	allScopeProperties, scopeToProperties := createPropertiesInstance()
	module.scopeProperties = scopeToProperties
	module.AddProperties(&module.properties, allScopeProperties)

	// Initialize information common between source and prebuilt.
	module.initCommon(&module.ModuleBase)

	android.InitPrebuiltModule(module, &[]string{""})
	android.InitApexModule(module)
	android.InitSdkAwareModule(module)
	InitJavaModule(module, android.HostAndDeviceSupported)

	module.SetDefaultableHook(func(mctx android.DefaultableHookContext) {
		if module.initCommonAfterDefaultsApplied(mctx) {
			module.createInternalModules(mctx)
		}
	})
	return module
}

func (module *sdkLibraryImport) Prebuilt() *android.Prebuilt {
	return &module.prebuilt
}

func (module *sdkLibraryImport) Name() string {
	return module.prebuilt.Name(module.ModuleBase.Name())
}

func (module *sdkLibraryImport) createInternalModules(mctx android.DefaultableHookContext) {

	// If the build is configured to use prebuilts then force this to be preferred.
	if mctx.Config().UnbundledBuildUsePrebuiltSdks() {
		module.prebuilt.ForcePrefer()
	}

	for apiScope, scopeProperties := range module.scopeProperties {
		if len(scopeProperties.Jars) == 0 {
			continue
		}

		module.createJavaImportForStubs(mctx, apiScope, scopeProperties)

		module.createPrebuiltStubsSources(mctx, apiScope, scopeProperties)
	}

	javaSdkLibraries := javaSdkLibraries(mctx.Config())
	javaSdkLibrariesLock.Lock()
	defer javaSdkLibrariesLock.Unlock()
	*javaSdkLibraries = append(*javaSdkLibraries, module.BaseModuleName())
}

func (module *sdkLibraryImport) createJavaImportForStubs(mctx android.DefaultableHookContext, apiScope *apiScope, scopeProperties *sdkLibraryScopeProperties) {
	// Creates a java import for the jar with ".stubs" suffix
	props := struct {
		Name                *string
		Soc_specific        *bool
		Device_specific     *bool
		Product_specific    *bool
		System_ext_specific *bool
		Sdk_version         *string
		Libs                []string
		Jars                []string
		Prefer              *bool
	}{}
	props.Name = proptools.StringPtr(module.stubsLibraryModuleName(apiScope))
	props.Sdk_version = scopeProperties.Sdk_version
	// Prepend any of the libs from the legacy public properties to the libs for each of the
	// scopes to avoid having to duplicate them in each scope.
	props.Libs = append(module.properties.Libs, scopeProperties.Libs...)
	props.Jars = scopeProperties.Jars
	if module.SocSpecific() {
		props.Soc_specific = proptools.BoolPtr(true)
	} else if module.DeviceSpecific() {
		props.Device_specific = proptools.BoolPtr(true)
	} else if module.ProductSpecific() {
		props.Product_specific = proptools.BoolPtr(true)
	} else if module.SystemExtSpecific() {
		props.System_ext_specific = proptools.BoolPtr(true)
	}
	// The imports are preferred if the java_sdk_library_import is preferred.
	props.Prefer = proptools.BoolPtr(module.prebuilt.Prefer())
	mctx.CreateModule(ImportFactory, &props)
}

func (module *sdkLibraryImport) createPrebuiltStubsSources(mctx android.DefaultableHookContext, apiScope *apiScope, scopeProperties *sdkLibraryScopeProperties) {
	props := struct {
		Name   *string
		Srcs   []string
		Prefer *bool
	}{}
	props.Name = proptools.StringPtr(module.stubsSourceModuleName(apiScope))
	props.Srcs = scopeProperties.Stub_srcs
	mctx.CreateModule(PrebuiltStubsSourcesFactory, &props)

	// The stubs source is preferred if the java_sdk_library_import is preferred.
	props.Prefer = proptools.BoolPtr(module.prebuilt.Prefer())
}

func (module *sdkLibraryImport) DepsMutator(ctx android.BottomUpMutatorContext) {
	for apiScope, scopeProperties := range module.scopeProperties {
		if len(scopeProperties.Jars) == 0 {
			continue
		}

		// Add dependencies to the prebuilt stubs library
		ctx.AddVariationDependencies(nil, apiScope.stubsTag, module.stubsLibraryModuleName(apiScope))
	}
}

func (module *sdkLibraryImport) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	// Record the paths to the prebuilt stubs library.
	ctx.VisitDirectDeps(func(to android.Module) {
		tag := ctx.OtherModuleDependencyTag(to)

		if lib, ok := to.(Dependency); ok {
			if scopeTag, ok := tag.(scopeDependencyTag); ok {
				apiScope := scopeTag.apiScope
				scopePaths := module.getScopePaths(apiScope)
				scopePaths.stubsHeaderPath = lib.HeaderJars()
			}
		}
	})
}

func (module *sdkLibraryImport) sdkJars(
	ctx android.BaseModuleContext,
	sdkVersion sdkSpec) android.Paths {

	// If a specific numeric version has been requested then use prebuilt versions of the sdk.
	if sdkVersion.version.isNumbered() {
		return PrebuiltJars(ctx, module.BaseModuleName(), sdkVersion)
	}

	var apiScope *apiScope
	switch sdkVersion.kind {
	case sdkSystem:
		apiScope = apiScopeSystem
	case sdkTest:
		apiScope = apiScopeTest
	default:
		apiScope = apiScopePublic
	}

	paths := module.getScopePaths(apiScope)
	return paths.stubsHeaderPath
}

// to satisfy SdkLibraryDependency interface
func (module *sdkLibraryImport) SdkHeaderJars(ctx android.BaseModuleContext, sdkVersion sdkSpec) android.Paths {
	// This module is just a wrapper for the prebuilt stubs.
	return module.sdkJars(ctx, sdkVersion)
}

// to satisfy SdkLibraryDependency interface
func (module *sdkLibraryImport) SdkImplementationJars(ctx android.BaseModuleContext, sdkVersion sdkSpec) android.Paths {
	// This module is just a wrapper for the stubs.
	return module.sdkJars(ctx, sdkVersion)
}

//
// java_sdk_library_xml
//
type sdkLibraryXml struct {
	android.ModuleBase
	android.DefaultableModuleBase
	android.ApexModuleBase

	properties sdkLibraryXmlProperties

	outputFilePath android.OutputPath
	installDirPath android.InstallPath
}

type sdkLibraryXmlProperties struct {
	// canonical name of the lib
	Lib_name *string
}

// java_sdk_library_xml builds the permission xml file for a java_sdk_library.
// Not to be used directly by users. java_sdk_library internally uses this.
func sdkLibraryXmlFactory() android.Module {
	module := &sdkLibraryXml{}

	module.AddProperties(&module.properties)

	android.InitApexModule(module)
	android.InitAndroidArchModule(module, android.DeviceSupported, android.MultilibCommon)

	return module
}

// from android.PrebuiltEtcModule
func (module *sdkLibraryXml) SubDir() string {
	return "permissions"
}

// from android.PrebuiltEtcModule
func (module *sdkLibraryXml) OutputFile() android.OutputPath {
	return module.outputFilePath
}

// from android.ApexModule
func (module *sdkLibraryXml) AvailableFor(what string) bool {
	return true
}

func (module *sdkLibraryXml) DepsMutator(ctx android.BottomUpMutatorContext) {
	// do nothing
}

// File path to the runtime implementation library
func (module *sdkLibraryXml) implPath() string {
	implName := proptools.String(module.properties.Lib_name)
	if apexName := module.ApexName(); apexName != "" {
		// TODO(b/146468504): ApexName() is only a soong module name, not apex name.
		// In most cases, this works fine. But when apex_name is set or override_apex is used
		// this can be wrong.
		return fmt.Sprintf("/apex/%s/javalib/%s.jar", apexName, implName)
	}
	partition := "system"
	if module.SocSpecific() {
		partition = "vendor"
	} else if module.DeviceSpecific() {
		partition = "odm"
	} else if module.ProductSpecific() {
		partition = "product"
	} else if module.SystemExtSpecific() {
		partition = "system_ext"
	}
	return "/" + partition + "/framework/" + implName + ".jar"
}

func (module *sdkLibraryXml) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	libName := proptools.String(module.properties.Lib_name)
	xmlContent := fmt.Sprintf(permissionsTemplate, libName, module.implPath())

	module.outputFilePath = android.PathForModuleOut(ctx, libName+".xml").OutputPath
	rule := android.NewRuleBuilder()
	rule.Command().
		Text("/bin/bash -c \"echo -e '" + xmlContent + "'\" > ").
		Output(module.outputFilePath)

	rule.Build(pctx, ctx, "java_sdk_xml", "Permission XML")

	module.installDirPath = android.PathForModuleInstall(ctx, "etc", module.SubDir())
}

func (module *sdkLibraryXml) AndroidMkEntries() []android.AndroidMkEntries {
	if !module.IsForPlatform() {
		return []android.AndroidMkEntries{android.AndroidMkEntries{
			Disabled: true,
		}}
	}

	return []android.AndroidMkEntries{android.AndroidMkEntries{
		Class:      "ETC",
		OutputFile: android.OptionalPathForPath(module.outputFilePath),
		ExtraEntries: []android.AndroidMkExtraEntriesFunc{
			func(entries *android.AndroidMkEntries) {
				entries.SetString("LOCAL_MODULE_TAGS", "optional")
				entries.SetString("LOCAL_MODULE_PATH", module.installDirPath.ToMakePath().String())
				entries.SetString("LOCAL_INSTALLED_MODULE_STEM", module.outputFilePath.Base())
			},
		},
	}}
}

type sdkLibrarySdkMemberType struct {
	android.SdkMemberTypeBase
}

func (s *sdkLibrarySdkMemberType) AddDependencies(mctx android.BottomUpMutatorContext, dependencyTag blueprint.DependencyTag, names []string) {
	mctx.AddVariationDependencies(nil, dependencyTag, names...)
}

func (s *sdkLibrarySdkMemberType) IsInstance(module android.Module) bool {
	_, ok := module.(*SdkLibrary)
	return ok
}

func (s *sdkLibrarySdkMemberType) AddPrebuiltModule(ctx android.SdkMemberContext, member android.SdkMember) android.BpModule {
	return ctx.SnapshotBuilder().AddPrebuiltModule(member, "java_sdk_library_import")
}

func (s *sdkLibrarySdkMemberType) CreateVariantPropertiesStruct() android.SdkMemberProperties {
	return &sdkLibrarySdkMemberProperties{}
}

type sdkLibrarySdkMemberProperties struct {
	android.SdkMemberPropertiesBase

	// Scope to per scope properties.
	Scopes map[*apiScope]scopeProperties

	// Additional libraries that the exported stubs libraries depend upon.
	Libs []string

	// The Java stubs source files.
	Stub_srcs []string

	// The naming scheme.
	Naming_scheme *string
}

type scopeProperties struct {
	Jars           android.Paths
	StubsSrcJar    android.Path
	CurrentApiFile android.Path
	RemovedApiFile android.Path
	SdkVersion     string
}

func (s *sdkLibrarySdkMemberProperties) PopulateFromVariant(ctx android.SdkMemberContext, variant android.Module) {
	sdk := variant.(*SdkLibrary)

	s.Scopes = make(map[*apiScope]scopeProperties)
	for _, apiScope := range allApiScopes {
		paths := sdk.getScopePaths(apiScope)
		jars := paths.stubsImplPath
		if len(jars) > 0 {
			properties := scopeProperties{}
			properties.Jars = jars
			properties.SdkVersion = sdk.sdkVersionForStubsLibrary(ctx.SdkModuleContext(), apiScope)
			properties.StubsSrcJar = paths.stubsSrcJar
			properties.CurrentApiFile = paths.currentApiFilePath
			properties.RemovedApiFile = paths.removedApiFilePath
			s.Scopes[apiScope] = properties
		}
	}

	s.Libs = sdk.properties.Libs
	s.Naming_scheme = sdk.commonProperties.Naming_scheme
}

func (s *sdkLibrarySdkMemberProperties) AddToPropertySet(ctx android.SdkMemberContext, propertySet android.BpPropertySet) {
	if s.Naming_scheme != nil {
		propertySet.AddProperty("naming_scheme", proptools.String(s.Naming_scheme))
	}

	for _, apiScope := range allApiScopes {
		if properties, ok := s.Scopes[apiScope]; ok {
			scopeSet := propertySet.AddPropertySet(apiScope.propertyName)

			scopeDir := filepath.Join("sdk_library", s.OsPrefix(), apiScope.name)

			var jars []string
			for _, p := range properties.Jars {
				dest := filepath.Join(scopeDir, ctx.Name()+"-stubs.jar")
				ctx.SnapshotBuilder().CopyToSnapshot(p, dest)
				jars = append(jars, dest)
			}
			scopeSet.AddProperty("jars", jars)

			// Merge the stubs source jar into the snapshot zip so that when it is unpacked
			// the source files are also unpacked.
			snapshotRelativeDir := filepath.Join(scopeDir, ctx.Name()+"_stub_sources")
			ctx.SnapshotBuilder().UnzipToSnapshot(properties.StubsSrcJar, snapshotRelativeDir)
			scopeSet.AddProperty("stub_srcs", []string{snapshotRelativeDir})

			if properties.CurrentApiFile != nil {
				currentApiSnapshotPath := filepath.Join(scopeDir, ctx.Name()+".txt")
				ctx.SnapshotBuilder().CopyToSnapshot(properties.CurrentApiFile, currentApiSnapshotPath)
				scopeSet.AddProperty("current_api", currentApiSnapshotPath)
			}

			if properties.RemovedApiFile != nil {
				removedApiSnapshotPath := filepath.Join(scopeDir, ctx.Name()+"-removed.txt")
				ctx.SnapshotBuilder().CopyToSnapshot(properties.CurrentApiFile, removedApiSnapshotPath)
				scopeSet.AddProperty("removed_api", removedApiSnapshotPath)
			}

			if properties.SdkVersion != "" {
				scopeSet.AddProperty("sdk_version", properties.SdkVersion)
			}
		}
	}

	if len(s.Libs) > 0 {
		propertySet.AddPropertyWithTag("libs", s.Libs, ctx.SnapshotBuilder().SdkMemberReferencePropertyTag(false))
	}
}
