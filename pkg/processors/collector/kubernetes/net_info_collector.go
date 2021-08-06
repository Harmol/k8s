package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/client"
	"github.com/go-logr/logr"
	"github.com/kubediag/kubediag/pkg/processors"
	"github.com/kubediag/kubediag/pkg/util"
	"io/ioutil"
	"k8s.io/klog"
	"net/http"
)

const timeOutSeconds int32 = 60

const (
	ContextKeyContainerIpaddr = "collector.kubernetes.container.ipaddr"
	ContextKeyContainerPid = "collector.kubernetes.container.Pid"
	ContextKeyContainerNat = "collector.kubernetes.container.Nat"
	ContextKeyContainerRoute = "collector.kubernetes.container.Route"
)

// NetInfoCollector manages information of all containers on the node.
type netInfoCollector struct {
	// Context carries values across API boundaries.
	context.Context
	// Logger represents the ability to log messages.
	logr.Logger

	// client is the API client that performs all operations against a docker server.
	client *client.Client
	// netInfoCollectorEnabled indicates whether containerCollector is enabled.
	netInfoCollectorEnabled bool
}

// NewNetInfoCollector creates a new containerCollector.
func NewNetInfoCollector(
	ctx context.Context,
	logger logr.Logger,
	dockerEndpoint string,
	netInfoCollectorEnabled bool,
) (processors.Processor, error) {
	cli, err := client.NewClientWithOpts(client.WithHost(dockerEndpoint))
	if err != nil {
		return nil, err
	}

	return &netInfoCollector{
		Context:                   ctx,
		Logger:                    logger,
		client:                    cli,
		netInfoCollectorEnabled: netInfoCollectorEnabled,
	}, nil
}


func (nc netInfoCollector) Handler(w http.ResponseWriter, r *http.Request) {
	if !nc.netInfoCollectorEnabled {
		http.Error(w, fmt.Sprintf("net info collector is not enabled"), http.StatusUnprocessableEntity)
		return
	}

	switch r.Method {
	case "POST":
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			http.Error(w, fmt.Sprintf("Unable to read request body: %v", err), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()

		fmt.Println(string(body))

		var parameters map[string]string
		err = json.Unmarshal(body, &parameters)
		if err != nil {
			http.Error(w, fmt.Sprintf("Unable to unmarshal request body: %v", err), http.StatusBadRequest)
			return
		}

		result := make(map[string]string)
		containerId, ok := parameters["id"]
		if ok {
			klog.Infof("Start collecting net information of container(id): %s.", containerId)

			klog.Infof("Start collecting Pid of container(id): %s.", containerId)
			out, err := util.BlockingRunCommandWithTimeout([]string{"docker", "inspect", "--format='{{.State.Pid}}'", containerId}, timeOutSeconds)
			if err != nil {
				result[ContextKeyContainerPid] = err.Error()
			} else {
				result[ContextKeyContainerPid] = string(out[1:len(out)-2])	// 去除头尾的'和\n
			}
			pid := result[ContextKeyContainerPid]

			// ip addr命令
			klog.Infof("Start collecting ip address. container(id): %s, pid: %s", containerId, pid)
			out, err = util.BlockingRunCommandWithTimeout([]string{"nsenter", "-t", pid, "-n", "ip", "addr"}, timeOutSeconds)
			if err != nil {
				result[ContextKeyContainerIpaddr] = err.Error()
			} else {
				result[ContextKeyContainerIpaddr] = string(out)
			}

			// route命令
			klog.Infof("Start collecting route. container(id): %s, pid: %s", containerId, pid)
			out, err = util.BlockingRunCommandWithTimeout([]string{"nsenter", "-t", pid, "route"}, timeOutSeconds)
			if err != nil {
				result[ContextKeyContainerRoute] = err.Error()
			} else {
				result[ContextKeyContainerRoute] = string(out)
			}

			// iptables-save命令
			klog.Infof("Start collecting iptables.")
			_,err = util.BlockingRunCommandWithTimeout([]string{"nsenter", "-t", pid, "iptables-save"}, timeOutSeconds)
			if err != nil {
				klog.Exit(err)
			}else {
				out, err = util.BlockingRunCommandWithTimeout([]string{"iptables-save", "-t", "nat"}, timeOutSeconds)
				if err != nil {
					result[ContextKeyContainerNat] = err.Error()
				} else {
					result[ContextKeyContainerNat] = string(out)
				}
			}


			data, err := json.Marshal(result)
			if err != nil {
				http.Error(w, fmt.Sprintf("failed to marshal result: %v", err), http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Write(data)
		} else {
			klog.Infof("Fail to get container id.")
		}
	default:
		http.Error(w, fmt.Sprintf("method %s is not supported", r.Method), http.StatusMethodNotAllowed)

	}
}