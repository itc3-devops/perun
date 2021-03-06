// Copyright 2017 Appliscale
//
// Maintainers and contributors are listed in README file inside repository.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package offlinevalidator provides tools for offline CloudFormation template
// validation.
package offlinevalidator

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"path"
	"reflect"
	"strconv"
	"strings"

	"github.com/Appliscale/perun/context"
	"github.com/Appliscale/perun/intrinsicsolver"
	"github.com/Appliscale/perun/logger"
	"github.com/Appliscale/perun/offlinevalidator/template"
	"github.com/Appliscale/perun/offlinevalidator/validators"
	"github.com/Appliscale/perun/specification"
	"github.com/awslabs/goformation"
	"github.com/awslabs/goformation/cloudformation"
	"github.com/ghodss/yaml"
	"github.com/mitchellh/mapstructure"
)

var validatorsMap = map[string]interface{}{
	"AWS::EC2::VPC": validators.IsVpcValid,
}

func printResult(valid *bool, logger *logger.Logger) {
	logger.PrintValidationErrors()
	if !*valid {
		logger.Error("Template is invalid!")
	} else {
		logger.Info("Template is valid!")
	}
}

// Validate CloudFormation template.
func Validate(context *context.Context) bool {
	valid := false
	defer printResult(&valid, context.Logger)

	specification, err := specification.GetSpecification(context)

	if err != nil {
		context.Logger.Error(err.Error())
		return false
	}

	rawTemplate, err := ioutil.ReadFile(*context.CliArguments.TemplatePath)
	if err != nil {
		context.Logger.Error(err.Error())
		return false
	}

	var perunTemplate template.Template
	var goFormationTemplate cloudformation.Template

	templateFileExtension := path.Ext(*context.CliArguments.TemplatePath)
	if templateFileExtension == ".json" {
		goFormationTemplate, err = parseJSON(rawTemplate, perunTemplate, context.Logger)
	} else if templateFileExtension == ".yaml" || templateFileExtension == ".yml" {
		goFormationTemplate, err = parseYAML(rawTemplate, perunTemplate, context.Logger)
	} else {
		err = errors.New("Invalid template file format.")
	}

	if err != nil {
		context.Logger.Error(err.Error())
		return false
	}

	deNilizedTemplate, _ := nilNeutralize(goFormationTemplate, context.Logger)
	resources := obtainResources(deNilizedTemplate, perunTemplate, context.Logger)
	deadResources := getNilResources(resources)
	deadProperties := getNilProperties(resources)

	valid = validateResources(resources, &specification, context.Logger, deadProperties, deadResources)
	return valid
}

func validateResources(resources map[string]template.Resource, specification *specification.Specification, sink *logger.Logger, deadProp []string, deadRes []string) bool {

	for resourceName, resourceValue := range resources {
		if deadResource := sliceContains(deadRes, resourceName); !deadResource {
			resourceValidation := sink.AddResourceForValidation(resourceName)

			if resourceSpecification, ok := specification.ResourceTypes[resourceValue.Type]; ok {
				for propertyName, propertyValue := range resourceSpecification.Properties {
					if deadProperty := sliceContains(deadProp, propertyName); !deadProperty {
						validateProperties(specification, resourceValue, propertyName, propertyValue, resourceValidation)
					}
				}
			} else {
				resourceValidation.AddValidationError("Type needs to be specified")
			}
			if validator, ok := validatorsMap[resourceValue.Type]; ok {
				validator.(func(template.Resource, *logger.ResourceValidation) bool)(resourceValue, resourceValidation)
			}

		}
	}
	return !sink.HasValidationErrors()
}

func validateProperties(
	specification *specification.Specification,
	resourceValue template.Resource,
	propertyName string,
	propertyValue specification.Property,
	resourceValidation *logger.ResourceValidation) {

	if _, ok := resourceValue.Properties[propertyName]; !ok {
		if propertyValue.Required {
			resourceValidation.AddValidationError("Property " + propertyName + " is required")
		}
	} else if len(propertyValue.Type) > 0 {
		if propertyValue.Type != "List" && propertyValue.Type != "Map" {
			checkNestedProperties(specification, resourceValue.Properties, resourceValue.Type, propertyName, propertyValue.Type, resourceValidation)
		} else if propertyValue.Type == "List" {
			checkListProperties(specification, resourceValue.Properties, resourceValue.Type, propertyName, propertyValue.ItemType, resourceValidation)
		} else if propertyValue.Type == "Map" {
			checkMapProperties(resourceValue.Properties, propertyName, resourceValidation)
		}
	}
}

func checkListProperties(
	spec *specification.Specification,
	resourceProperties map[string]interface{},
	resourceValueType, propertyName, listItemType string,
	resourceValidation *logger.ResourceValidation) {

	if listItemType == "" {
		resourceSubproperties := toStringList(resourceProperties, propertyName)
		if reflect.TypeOf(resourceSubproperties).Kind() != reflect.Slice || len(resourceSubproperties) == 0 {
			resourceValidation.AddValidationError(propertyName + " must be a List")
		}
	} else if propertySpec, hasSpec := spec.PropertyTypes[resourceValueType+"."+listItemType]; hasSpec {

		resourceSubproperties := toMapList(resourceProperties, propertyName)
		for subpropertyName, subpropertyValue := range propertySpec.Properties {
			for _, listItem := range resourceSubproperties {
				if _, isPresent := listItem[subpropertyName]; !isPresent {
					if subpropertyValue.Required {
						resourceValidation.AddValidationError("Property " + subpropertyName + " is required in " + listItemType)
					}
				} else if isPresent {
					if subpropertyValue.IsSubproperty() {
						checkNestedProperties(spec, listItem, resourceValueType, subpropertyName, subpropertyValue.Type, resourceValidation)
					} else if subpropertyValue.Type == "Map" {
						checkMapProperties(listItem, propertyName, resourceValidation)
					}
				}
			}
		}
	}
}

func checkNestedProperties(
	spec *specification.Specification,
	resourceProperties map[string]interface{},
	resourceValueType, propertyName, propertyType string,
	resourceValidation *logger.ResourceValidation) {

	if propertySpec, hasSpec := spec.PropertyTypes[resourceValueType+"."+propertyType]; hasSpec {
		resourceSubproperties, _ := toMap(resourceProperties, propertyName)
		for subpropertyName, subpropertyValue := range propertySpec.Properties {
			if _, isPresent := resourceSubproperties[subpropertyName]; !isPresent {
				if subpropertyValue.Required {
					resourceValidation.AddValidationError("Property " + subpropertyName + " is required" + "in " + propertyName)
				}
			} else if isPresent {
				if subpropertyValue.IsSubproperty() {
					checkNestedProperties(spec, resourceSubproperties, resourceValueType, subpropertyName, subpropertyValue.Type, resourceValidation)
				} else if subpropertyValue.Type == "List" {
					checkListProperties(spec, resourceSubproperties, resourceValueType, subpropertyName, subpropertyValue.ItemType, resourceValidation)
				} else if subpropertyValue.Type == "Map" {
					checkMapProperties(resourceSubproperties, subpropertyName, resourceValidation)
				}
			}
		}
	}
}

func checkMapProperties(
	resourceProperties map[string]interface{},
	propertyName string,
	resourceValidation *logger.ResourceValidation) {

	_, err := toMap(resourceProperties, propertyName)
	if err != nil {
		resourceValidation.AddValidationError(err.Error())
	}
}

func parseJSON(templateFile []byte, refTemplate template.Template, logger *logger.Logger) (template cloudformation.Template, err error) {

	err = json.Unmarshal(templateFile, &refTemplate)
	if err != nil {
		if syntaxError, isSyntaxError := err.(*json.SyntaxError); isSyntaxError {
			syntaxOffset := int(syntaxError.Offset)
			line, character := lineAndCharacter(string(templateFile), syntaxOffset)
			logger.Error("Syntax error at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(character))
		} else if typeError, isTypeError := err.(*json.UnmarshalTypeError); isTypeError {
			typeOffset := int(typeError.Offset)
			line, character := lineAndCharacter(string(templateFile), typeOffset)
			logger.Error("Type error at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(character))
		}
		return template, err
	}

	tempJSON, err := goformation.ParseJSON(templateFile)
	if err != nil {
		logger.Error(err.Error())
	}

	returnTemplate := *tempJSON

	return returnTemplate, nil
}

func parseYAML(templateFile []byte, refTemplate template.Template, logger *logger.Logger) (template cloudformation.Template, err error) {

	err = yaml.Unmarshal(templateFile, &refTemplate)
	if err != nil {
		return template, err
	}

	preprocessed, preprocessingError := intrinsicsolver.FixFunctions(templateFile, logger, "multiline", "elongate", "correctlong")
	if preprocessingError != nil {
		logger.Error(preprocessingError.Error())
	}
	tempYAML, err := goformation.ParseYAML(preprocessed)
	if err != nil {
		logger.Error(err.Error())
	}

	returnTemplate := *tempYAML

	return returnTemplate, nil
}

func obtainResources(goformationTemplate cloudformation.Template, perunTemplate template.Template, logger *logger.Logger) map[string]template.Resource {
	perunResources := perunTemplate.Resources
	goformationResources := goformationTemplate.Resources

	mapstructure.Decode(goformationResources, &perunResources)

	for propertyName, propertyContent := range perunResources {
		if propertyContent.Properties == nil {
			logger.Debug(propertyName + " <--- is nil.")
		} else {
			for element, elementValue := range propertyContent.Properties {
				if elementValue == nil {
					logger.Debug(propertyName + ": " + element + " <--- is nil.")
				} else if elementMap, ok := elementValue.(map[string]interface{}); ok {
					for key, value := range elementMap {
						if value == nil {
							logger.Debug(propertyName + ": " + element + ": " + key + " <--- is nil.")
						} else if elementOfElement, ok := value.(map[string]interface{}); ok {
							for subKey, subValue := range elementOfElement {
								if subValue == nil {
									logger.Debug(propertyName + ": " + element + ": " + key + ": " + subKey + " <--- is nil.")
								}
							}
						} else if sliceOfElement, ok := value.([]interface{}); ok {
							for indexKey, indexValue := range sliceOfElement {
								if indexValue == nil {
									logger.Debug(propertyName + ": " + element + ": " + key + "[" + strconv.Itoa(indexKey) + "] <--- is nil.")
								}
							}
						}
					}
				} else if elementSlice, ok := elementValue.([]interface{}); ok {
					for index, value := range elementSlice {
						if value == nil {
							logger.Debug(propertyName + ": " + element + "[" + strconv.Itoa(index) + "] <--- is nil.")
						} else if elementOfElement, ok := value.(map[string]interface{}); ok {
							for subKey, subValue := range elementOfElement {
								if subValue == nil {
									logger.Debug(propertyName + ": " + element + "[" + strconv.Itoa(index) + "]: " + subKey + " <--- is nil.")
								}
							}
						} else if sliceOfElement, ok := value.([]interface{}); ok {
							for indexKey, indexValue := range sliceOfElement {
								if indexValue == nil {
									logger.Debug(propertyName + ": " + element + "[" + strconv.Itoa(index) + "][" + strconv.Itoa(indexKey) + "] <--- is nil.")
								}
							}
						}
					}
				}
			}
		}
	}

	return perunResources
}

func toMapList(resourceProperties map[string]interface{}, propertyName string) []map[string]interface{} {
	subproperties, ok := resourceProperties[propertyName].([]interface{})
	if !ok {
		return []map[string]interface{}{}
	}
	mapList := make([]map[string]interface{}, len(subproperties))
	for index, value := range subproperties {
		if _, ok := value.(map[string]interface{}); ok {
			mapList[index] = value.(map[string]interface{})
		}
	}
	return mapList
}

func toStringList(resourceProperties map[string]interface{}, propertyName string) []string {
	subproperties, ok := resourceProperties[propertyName].([]interface{})
	if !ok {
		return nil
	}

	list := make([]string, len(subproperties))
	for index, value := range subproperties {
		if value != nil {
			list[index] = value.(string)
		}
	}
	return list
}

func toMap(resourceProperties map[string]interface{}, propertyName string) (map[string]interface{}, error) {
	subproperties, ok := resourceProperties[propertyName].(map[string]interface{})
	if !ok {
		return nil, errors.New(propertyName + " must be a Map")
	}
	return subproperties, nil
}

// There is a possibility that a hash map inside the template would have one of it's element's being an intrinsic function designed to output `key : value` pair.
// If this function would be unresolved, it would output a standalone <nil> of type interface{}. It would be an alien element in a hash map.
// To prevent the parser from breaking, we wipe out the entire, expected hash map element.
func nilNeutralize(template cloudformation.Template, logger *logger.Logger) (output cloudformation.Template, err error) {
	bytes, initErr := json.Marshal(template)
	if initErr != nil {
		logger.Error(err.Error())
	}
	byteSlice := string(bytes)

	var info int
	var check1, check2, check3 string
	if strings.Contains(byteSlice, ",null,") {
		check1 = strings.Replace(byteSlice, ",null,", ",", -1)
		info++
	} else {
		check1 = byteSlice
	}
	if strings.Contains(check1, "[null,") {
		check2 = strings.Replace(check1, "[null,", "[", -1)
		info++
	} else {
		check2 = check1
	}
	if strings.Contains(check2, ",null]") {
		check3 = strings.Replace(check2, ",null]", "]", -1)
		info++
	} else {
		check3 = check2
	}

	byteSliceCorrected := []byte(check3)

	tempJSON, err := goformation.ParseJSON(byteSliceCorrected)
	if err != nil {
		logger.Error(err.Error())
	}

	infoOpening, link, part, occurences, elements, a, t := "", "", "", "", "", "", ""
	if info > 0 {
		if info == 1 {
			elements = "element"
			t = "this "
			a = "a"
			infoOpening = "is an intrinsic function "
			link = "is"
			part = "part"
		} else {
			elements = "elements"
			t = "those "
			occurences = strconv.Itoa(info)
			infoOpening = "are " + occurences + " intrinsic functions "
			link = "are"
			part = "parts"
		}
		logger.Info("There " + infoOpening + "which would output `key : value` pair but " + link + " unresolved and " + link + " evaluated to <nil>. As " + t + elements + " of a template should be " + a + " hash table " + elements + ", " + t + "standalone <nil> " + link + " deleted completely. It is recommended to investigate " + t + part + " of a template manually.")
	}

	returnTemplate := *tempJSON

	return returnTemplate, nil
}

func getNilProperties(resources map[string]template.Resource) []string {
	list := make([]string, 0)
	for _, resourceContent := range resources {
		properties := resourceContent.Properties
		for propertyName, propertyContent := range properties {
			if propertyContent == nil {
				list = append(list, propertyName)
			}
		}
	}
	return list
}

func getNilResources(resources map[string]template.Resource) []string {
	list := make([]string, 0)
	for resourceName, resourceContent := range resources {
		if resourceContent.Properties == nil {
			list = append(list, resourceName)
		}

	}
	return list
}

func sliceContains(slice []string, match string) bool {
	for _, s := range slice {
		if s == match {
			return true
		}
	}
	return false
}

func lineAndCharacter(input string, offset int) (line int, character int) {
	lf := rune(0x0A)

	if offset > len(input) || offset < 0 {
		return 0, 0
	}

	line = 1

	for i, b := range input {
		if b == lf {
			if i < offset {
				line++
				character = 0
			}
		} else {
			character++
		}
		if i == offset {
			break
		}
	}
	return line, character
}
