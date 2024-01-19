# Virtual Kubelet Lab

Personal lab for experimenting with [Virtual Kubelet][vkubelet] using a mocked provider.

## Usage

```
go build .
```

```
./vkubelet -klog.v=4
```

```
kubectl create -f example-pod.yaml
```

[vkubelet]: https://github.com/virtual-kubelet/virtual-kubelet
