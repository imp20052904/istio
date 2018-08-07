// Copyright 2016 Istio Authors
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

package main

import (
	"os"

	"istio.io/istio/mixer/adapter"
	"istio.io/istio/mixer/cmd/mixs/cmd"
	"istio.io/istio/mixer/cmd/shared"
	adptr "istio.io/istio/mixer/pkg/adapter"
	"istio.io/istio/mixer/pkg/template"
	generatedTmplRepo "istio.io/istio/mixer/template"
)

// 从mixer/pkg/template包获取所有注册的模板信息。
func supportedTemplates() map[string]template.Info {
	return generatedTmplRepo.SupportedTmplInfo
}

// 从mixer/pkg/adapter包获取所有注册的适配器信息。
func supportedAdapters() []adptr.InfoFn {
	return adapter.Inventory()
}
// Mixs命令入口
func main() {
	// 构造cobra.Command实例，mixs server子命令设计在serverCmd中定义。
	rootCmd := cmd.GetRootCmd(os.Args[1:], supportedTemplates(), supportedAdapters(), shared.Printf, shared.Fatalf)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(-1)
	}
}
