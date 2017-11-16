// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
	redis "gopkg.in/redis.v4"
	yaml "gopkg.in/yaml.v2"

	"github.com/prometheus/common/version"
	"github.com/tinytub/rules_adapter/pkg/rulefmt"
)

func main() {
	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling for the Prometheus monitoring system.")
	app.Version(version.Print("promtool"))
	app.HelpFlag.Short('h')

	updateCmd := app.Command("update", "Update the resources to newer formats.")
	ruleFilePath := updateCmd.Arg("path", "rules file path").Required().ExistingDir()
	redisPath := updateCmd.Arg("redis", "redis path ip:port").Required().TCP()
	redisPassword := updateCmd.Arg("password", "redis path password").Required().String()

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case updateCmd.FullCommand():
		os.Exit(RefreshRules(*ruleFilePath, (*redisPath).String(), *redisPassword))
	}

}

type judgeRecored struct {
	Name string `json:"alarm_name"`
	Expr string `json:"expre"`
	Step string `json:"step"`
}

func RefreshRules(path, redis, password string) int {

	interval := time.Duration(5 * time.Second)
	updateRules(path, redis, password)

	for {
		select {
		case <-time.Tick(interval):
			updateRules(path, redis, password)
		}
	}

}

func updateRules(fpath, redis, password string) int {
	//TODO: filename with job or service name
	filename := "test"
	abpath := path.Join(fpath, filename+".yml")
	absfpath, _ := filepath.Abs(abpath)

	data, err := getRedisData(redis, password)
	if err != nil {
		fmt.Println("get data from redis error: ", err)
		return 1
	}

	remoteRulesMap, err := getRemoteRules(data)
	if err != nil {
		fmt.Println("get Remote rules error: ", err)
		return 1
	}

	//check rules
	rulenum, ruleGroups, errsLocal := checkLocalRules(absfpath)

	//TODO: 文件不存在的时候该如何处理？
	if errsLocal != nil {
		fmt.Println("local rules err: ", errsLocal)
		return 1
	}

	localrules := make([]rulefmt.Rule, 0, rulenum)
	for _, rg := range ruleGroups {
		localrules = append(localrules, rg.Rules...)
	}

	_, remoteRules, err := convertToYaml(remoteRulesMap, filename)
	/*
		errsRemote := checkRulesValid(remoteRules)

		if errsRemote != nil {
			fmt.Println("remote rules err: ", errsRemote)
			return 1
		}
	*/

	//ioutil.WriteFile(absfpath)

	updates, nlocalrules := checkUpdate(localrules, remoteRules)

	//update 处理方式应该不一样
	//localrules = append(localrules, newupdate)

	//nlocalrules = append(localrules, newrules)

	//y, remoteRules, err := convertToYaml(nlocalrules, filename)

	fmt.Println("newlocalrules:", nlocalrules)
	yamlRG := &rulefmt.RuleGroups{
		Groups: []rulefmt.RuleGroup{{
			Name: filename,
		}},
	}
	yamlRG.Groups[0].Rules = nlocalrules
	y, err := yaml.Marshal(yamlRG)

	if err != nil {
		fmt.Println("yaml marshal error:", err)
		return 1
	}

	isUpdate := false
	if updates > 0 {
		isUpdate = true
	}

	if isUpdate {
		//TODO: 暂时比较粗暴，rule全刷，如果碰到已存在配置文件的情况可能会把已有内容刷丢。
		//尽量配合newrules和newupdate列表进行变更。
		updateRulesFile(y, absfpath)
		reloadPromeConfig()
	}

	return 0

}

func getRedisData(path, password string) ([]string, error) {
	client := redis.NewClient(&redis.Options{
		Network:  "tcp",
		Addr:     path,
		Password: password,
		DB:       0,
	})
	defer client.Close()

	data, err := client.LRange("CUSTOM_EXPRESS_STRATEGY", 0, -1).Result()
	if err != nil {
		fmt.Println(err)
		return nil, err
	}
	fmt.Println("get redis data: ", data)
	return data, nil
}

func getRemoteRules(data []string) (map[string]string, error) {
	remoteRulesMap := make(map[string]string, len(data))
	for _, d := range data {
		var record judgeRecored
		err := json.Unmarshal([]byte(d), &record)
		if err != nil {
			return nil, err
		}
		remoteRulesMap[record.Name] = strings.TrimSpace(record.Expr)
	}

	return remoteRulesMap, nil
}

func convertToYaml(remoteRules map[string]string, filename string) ([]byte, []rulefmt.Rule, error) {
	yamlRG := &rulefmt.RuleGroups{
		Groups: []rulefmt.RuleGroup{{
			Name: filename,
		}},
	}

	yamlRules := make([]rulefmt.Rule, 0, len(remoteRules))

	for name, expr := range remoteRules {
		yamlRules = append(yamlRules, rulefmt.Rule{
			Record: name,
			Expr:   expr,
			//Labels: map[string]string{"testlable1": "testvalue1", "testlabel2": "testvalue2"},
		})
	}

	yamlRG.Groups[0].Rules = yamlRules
	y, err := yaml.Marshal(yamlRG)

	if err != nil {
		fmt.Println(err)
		return nil, nil, err
	}
	return y, yamlRules, err
}

func checkRulesValid(data []rulefmt.Rule) []error {
	var errors []error
	for _, d := range data {
		errs := d.Validate()
		if errs != nil {
			errors = append(errors, errs...)
		}
	}
	return errors
}

func checkLocalRules(filename string) (int, []rulefmt.RuleGroup, []error) {
	fmt.Println("Checking", filename)
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		f, _ := os.OpenFile(filename, os.O_RDONLY|os.O_CREATE, 0666)
		f.Close()
	}
	rgs, errs := rulefmt.ParseFile(filename)
	if errs != nil {
		return 0, nil, errs
	}

	numRules := 0
	for _, rg := range rgs.Groups {
		numRules += len(rg.Rules)
	}

	return numRules, rgs.Groups, nil
}

func checkUpdate(localrule, remoterule []rulefmt.Rule) (int, []rulefmt.Rule) {
	var newrules []string
	var newupdates []string
	var deletedrules []string
	var nrule bool
	var newlocalrule []rulefmt.Rule

	for _, rrule := range remoterule {
		if err := rrule.Validate(); err != nil {
			continue
		}
		newlocalrule = append(newlocalrule, rrule)

		//check update && new
		nrule = true
		for _, lrule := range localrule {
			if lrule.Record == rrule.Record {
				nrule = false
				if lrule.Expr != rrule.Expr || !reflect.DeepEqual(lrule.Labels, rrule.Labels) {
					//newupdate 是不是可以去掉
					newupdates = append(newupdates, rrule.Record)
					//localrule[n] = rrule
					break
				}
				break
			}
		}
		if nrule {
			newrules = append(newrules, rrule.Record)
			//localrule = append(localrule, rrule)
		}
	}

	//check deleted
	for _, orule := range localrule {
		deleted := true
		for _, rule := range newlocalrule {
			if orule.Record == rule.Record {
				deleted = false
			}
		}
		if deleted == true {
			deletedrules = append(deletedrules, orule.Record)
		}
	}

	fmt.Println("newrules: ", newrules)
	fmt.Println("deletedrules: ", deletedrules)
	fmt.Println("newupdates: ", newupdates)

	updates := len(newrules) + len(newupdates) + len(deletedrules)

	//return newrules, newupdates, newlocalrule
	return updates, newlocalrule
}

func updateRulesFile(data []byte, filename string) {
	fmt.Println(filename)
	ioutil.WriteFile(filename, data, 0666)
}

func reloadPromeConfig() {
	/*
		client, err := NewClientForTimeOut()
		if err != nil {
			fmt.Println(err)
			return
		}
		url := "http://127.0.0.1:9090/-/reload"
		request, err := http.NewRequest("GET", url, strings.NewReader(""))
		if err != nil {
			fmt.Println(err)
			return
		}

		_, err = client.Do(request)

		if err != nil {
			fmt.Println(err)
			return
		}
	*/
	//_, err := exec.Command("sh", "pkill -SIGHUP prometheus").Output()
	_, err := exec.Command("sh", "-c", "pkill -SIGHUP prometheus").Output()
	if err != nil {
		fmt.Println("prometheus reload Failed", err)
	}

	fmt.Println("prometheus reload Done")
}

func NewClientForTimeOut() (*http.Client, error) {

	timeout := time.Duration(3 * time.Second)
	var rt http.RoundTripper = NewDeadlineRoundTripper(timeout)

	// Return a new client with the configured round tripper.
	return NewClient(rt), nil
}

func NewDeadlineRoundTripper(timeout time.Duration) http.RoundTripper {
	return &http.Transport{
		DisableKeepAlives: true,
		Dial: func(netw, addr string) (c net.Conn, err error) {
			start := time.Now()

			c, err = net.DialTimeout(netw, addr, timeout)
			if err != nil {
				return nil, err
			}

			//TODO 超时打点
			if err = c.SetDeadline(start.Add(timeout)); err != nil {
				c.Close()
				return nil, err
			}

			return c, nil
		},
	}
}

func NewClient(rt http.RoundTripper) *http.Client {
	return &http.Client{Transport: rt}
}
