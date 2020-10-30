package kubernetes

import (
    "encoding/json"
    "errors"
    "log"
    "io"
    "os"
    "regexp"
    "sync"
    "strings"
    "time"
    "path/filepath"
    "github.com/fsnotify/fsnotify"
    "github.com/akkeris/logtrain/internal/storage"
	"github.com/trevorlinton/go-tail/follower"
	"github.com/papertrail/remote_syslog2/syslog"
	api "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const kubeTime = "2006-01-02T15:04:05.000000000Z"

type kubeLine struct {
	Log string `json:"log"`
	Stream string `json:"stream"`
	Time string `json:"time"`
}

type kubeDetails struct {
	Container string
	DockerId string
	Namespace string
	Pod string
}

type fileWatcher struct {
	errors uint32
	follower *follower.Follower
	hostname string
	stop chan struct{}
	tag string
}


type hostnameAndTag struct {
	Hostname string
	Tag string
}

type Kubernetes struct {
	kube kubernetes.Interface
	closing bool
	errors chan error
	followers map[string]fileWatcher
	followersMutex sync.Mutex
	packets chan syslog.Packet
	path string
	watcher *fsnotify.Watcher
}


func getTopLevelObject(kube kubernetes.Interface, obj api.Object) (api.Object, error) {
	refs := obj.GetOwnerReferences()
	for _, ref := range refs {
		if ref.Controller == nil || *ref.Controller == false {
			if strings.ToLower(ref.Kind) == "replicaset" || strings.ToLower(ref.Kind) == "replicasets" {
				nObj, err := kube.AppsV1().ReplicaSets(obj.GetNamespace()).Get(ref.Name, api.GetOptions{})
				if err != nil {
					return nil, err
				}
				return getTopLevelObject(kube, nObj)
			} else if strings.ToLower(ref.Kind) == "deployment" || strings.ToLower(ref.Kind) == "deployments" {
				nObj, err := kube.AppsV1().Deployments(obj.GetNamespace()).Get(ref.Name, api.GetOptions{})
				if err != nil {
					return nil, err
				}
				return getTopLevelObject(kube, nObj)
			} else if strings.ToLower(ref.Kind) == "daemonset" || strings.ToLower(ref.Kind) == "daemonsets" {
				nObj, err := kube.AppsV1().DaemonSets(obj.GetNamespace()).Get(ref.Name, api.GetOptions{})
				if err != nil {
					return nil, err
				}
				return getTopLevelObject(kube, nObj)
			} else if strings.ToLower(ref.Kind) == "statefulset" || strings.ToLower(ref.Kind) == "statefulsets" {
				nObj, err := kube.AppsV1().StatefulSets(obj.GetNamespace()).Get(ref.Name, api.GetOptions{})
				if err != nil {
					return nil, err
				}
				return getTopLevelObject(kube, nObj)
			} else {
				return nil, errors.New("unrecognized object type " + ref.Kind)
			}
		}
	}
	return obj, nil
}

func deriveHostnameFromPod(podName string, podNamespace string, useAkkerisHosts bool) *hostnameAndTag {
	parts := strings.Split(podName, "-")
	if useAkkerisHosts {
		podId := strings.Join(parts[len(parts)-2:], "-")
		appAndDyno := strings.SplitN(strings.Join(parts[:len(parts)-2], "-"), "--", 2)
		if len(appAndDyno) < 2 {
			appAndDyno = append(appAndDyno, "web")
		}
		return &hostnameAndTag{
			Hostname: appAndDyno[0] + "-" + podNamespace,
			Tag: appAndDyno[1] + "." + podId,
		}
	}
	return &hostnameAndTag{
		Hostname: strings.Join(parts[:len(parts)-2], "-") + "." + podNamespace,
		Tag: podName,
	}
}

func akkerisGetTag(parts []string) string {
	podId := strings.Join(parts[len(parts)-2:], "-")
	appAndDyno := strings.SplitN(strings.Join(parts[:len(parts)-2], "-"), "--", 2)
	if len(appAndDyno) < 2 {
		appAndDyno = append(appAndDyno, "web")
	}
	return appAndDyno[1] + "." + podId
}

func getHostnameAndTagFromPod(kube kubernetes.Interface, obj api.Object, useAkkerisHosts bool) *hostnameAndTag {
	parts := strings.Split(obj.GetName(), "-")
	if host, ok := obj.GetAnnotations()[storage.HostnameAnnotationKey]; ok {
		if tag, ok := obj.GetAnnotations()[storage.TagAnnotationKey]; ok {
			return &hostnameAndTag{
				Hostname: host,
				Tag: tag,
			}
		} else {
			if useAkkerisHosts == true {
				return &hostnameAndTag{
					Hostname: host,
					Tag: akkerisGetTag(parts),
				}
			} else {
				return &hostnameAndTag{
					Hostname: host,
					Tag: obj.GetName(),
				}
			}
		}
	}
	top, err := getTopLevelObject(kube, obj)
	if err != nil {
		log.Printf("Unable to get top level object for obj %s/%s/%s due to %s", obj.GetResourceVersion(), obj.GetNamespace(), obj.GetName(), err.Error())
		return deriveHostnameFromPod(obj.GetName(), obj.GetNamespace(), useAkkerisHosts)
	}
	if useAkkerisHosts == true {
		appName, ok1 := top.GetLabels()[storage.AkkerisAppLabelKey]
		dynoType, ok2 := top.GetLabels()[storage.AkkerisDynoTypeLabelKey]
		if ok1 && ok2 {
			podId := strings.Join(parts[len(parts)-2:], "-")
			return &hostnameAndTag{
				Hostname: appName + "-" + obj.GetNamespace(),
				Tag: dynoType + "." + podId,
			}
		}
		return &hostnameAndTag{
			Hostname: top.GetName() + "-" + obj.GetNamespace(),
			Tag: akkerisGetTag(parts),
		}
	}
	return &hostnameAndTag{
		Hostname: top.GetName() + "." + obj.GetNamespace(),
		Tag: obj.GetName(),
	}
}

func dir(root string) []string {
    var files []string
    err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
        if(info.IsDir() == false) {
            files = append(files, path)
        }
        return nil
    })
    if err != nil {
        panic(err)
    }
    return files
}

func (handler *Kubernetes) Close() error {
	handler.closing = true 
	handler.watcher.Close()
	for _, v := range handler.followers {
		v.follower.Close()
	}
	close(handler.packets)
	close(handler.errors)
	return nil
}

func (handler *Kubernetes) Dial() error {
	if handler.watcher != nil {
		return errors.New("Dial may only be called once.")
	}
	for _, file := range dir(handler.path) {
		/* Seek the end of the file if we've just started, 
		 * if say we're erroring and restarting frequently we
		 * do not want to start from the beginning of the log file
		 * and rebroadcast the entire log contents
		 */
		handler.add(file, io.SeekEnd)
	}
	watcher, err := handler.watcherEventLoop()
	if err != nil {
		return err
	}
	handler.watcher = watcher
	return nil
}

func (handler *Kubernetes) Errors() (chan error) {
	return handler.errors
}

func (handler *Kubernetes) Packets() (chan syslog.Packet) {
	return handler.packets
}

func (handler *Kubernetes) Pools() bool {
	return true
}

func kubeDetailsFromFileName(file string) (*kubeDetails, error) {
	re := regexp.MustCompile(`(?P<pod_name>([a-z0-9][-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*)_(?P<namespace>[^_]+)_(?P<container_name>.+)-(?P<docker_id>[a-z0-9]{64})\.log$`)
	file = filepath.Base(file)
	if re.MatchString(file) {
		parts := re.FindAllStringSubmatch(file, -1)
		if len(parts[0]) != 8 {
			return nil, errors.New("invalid filename, too many parts")
		}
		return &kubeDetails{
			Container: parts[0][6],
			DockerId: parts[0][7],
			Namespace: parts[0][5],
			Pod: parts[0][1],
		}, nil
	}
	return nil, errors.New("invalid filename, no match given")
}

func (handler *Kubernetes) add(file string, ioSeek int) {
	config := follower.Config{
		Offset:0,
		Whence:ioSeek,
		Reopen:true,
	}
	// The filename has additional details we need.
	details, err := kubeDetailsFromFileName(file)
	if err != nil {
		return
	}

	useAkkerisHosts := os.Getenv("AKKERIS") == "true"
	hostAndTag := deriveHostnameFromPod(details.Pod, details.Namespace, useAkkerisHosts)
	pod, err := handler.kube.CoreV1().Pods(details.Namespace).Get(details.Pod, api.GetOptions{})
	if err != nil {
		log.Printf("Unable to get pod details from kubernetes for pod %#+v due to %s\n", details, err.Error())
	} else {
		hostAndTag = getHostnameAndTagFromPod(handler.kube, pod, useAkkerisHosts)
	}
	proc, err := follower.New(file, config)
	if err != nil {
		handler.Errors() <- err
		return
	}
	fw := fileWatcher{
		follower: proc,
		stop: make(chan struct{}, 1),
		hostname: hostAndTag.Hostname,
		tag: hostAndTag.Tag,
		errors: 0,
	}
	handler.followersMutex.Lock()
	handler.followers[file] = fw
	handler.followersMutex.Unlock()
	go func() {
		for {
			select {
			case line, ok := <-fw.follower.Lines():
				if ok == true {
					var data kubeLine
					if err := json.Unmarshal([]byte(line.String()), &data); err != nil {
						// track errors with the lines, but don't report anything.
						// TODO: should we do more? we shouldnt report this on
						// the kubernetes error handler as one corrupted file could make
						// the entire input handler look broken. Brainstorm on this.
						fw.errors++
					} else {
	    				t, err := time.Parse(kubeTime, data.Time)
	    				if err != nil {
	    					t = time.Now()
	    				}
						handler.Packets() <- syslog.Packet{
							Severity: 0,
							Facility: 0,
							Time:     t,
							Hostname: fw.hostname,
							Tag:      fw.tag,
							Message:  data.Log,
						}
					}
				} else {
					if fw.follower.Err() != nil {
						// track errors with the lines, but don't report anything.
						// TODO: should we do more? we shouldnt report this on
						// the kubernetes error handler as one corrupted file could make
						// the entire input handler look broken. Brainstorm on this.
						fw.errors++
					}
				}
			case <-fw.stop:
				return
			}
		}
	}()
}

func (handler *Kubernetes) watcherEventLoop() (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := watcher.Add(handler.path); err != nil {
		return nil, err
	}
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Create == fsnotify.Create {
					handler.add(event.Name, io.SeekStart)
				} else if event.Op&fsnotify.Remove == fsnotify.Remove {
					select {
					case handler.followers[event.Name].stop <- struct{}{}:
					default:
					}
					handler.followersMutex.Lock()
					delete(handler.followers, event.Name)
					handler.followersMutex.Unlock()
					return
				}
			case err := <-watcher.Errors:
				if handler.closing {
					return
				}
				select {
				case handler.Errors() <- err:
				default:
				}
			}
		}
	}()
	return watcher, nil
}

func Create(logpath string, kube kubernetes.Interface) (*Kubernetes, error) {
	if logpath == "" {
		logpath = "/var/log/containers"
	}
	return &Kubernetes{
		kube: kube,
		errors: make(chan error, 1),
		packets: make(chan syslog.Packet, 100),
		path: logpath,
		followers: make(map[string]fileWatcher),
		closing: false,
		followersMutex: sync.Mutex{},
	}, nil
}