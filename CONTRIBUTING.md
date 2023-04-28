## Development of the providers
### Running on the cluster
1. Clone the repo of interest:
```sh
git clone https://github.com/charmed-kubernetes/cluster-api-provider-juju.git
cd cluster-api-provider-juju
```

2. If needed, build and push the manager image to the location specified by `IMG` (if `IMG` is left unset it will attempt to push the image into rocks using the `latest` tag, as this is the default value for the variable defined in the Makefile):
```sh
make docker-build docker-push IMG=<some-registry>/capi-juju-controller:tag
```

3. Install Custom Resources:
```sh
make install
```

4. Deploy the controller to the cluster with the image specified by `DEPLOY_IMG` (if left unset it will use the default value for `DEPLOY_IMG` defined in the Makefile):
```sh
make deploy DEPLOY_IMG=<some-registry>/capi-juju-controller:tag
```

5. Verify the cluster and machine controllers have started by checking the controller-manager logs:

Check the deployment logs:
```sh
kubectl logs deployment/capi-juju-controller-manager -n capi-juju-system
```

You should see output similar to the following:
```sh
1.6733800930860527e+09	INFO	controller-runtime.metrics	Metrics server is starting to listen	{"addr": "127.0.0.1:8080"}
1.6733800930883508e+09	INFO	setup	starting manager
1.6733800930890276e+09	INFO	Starting server	{"path": "/metrics", "kind": "metrics", "addr": "127.0.0.1:8080"}
1.6733800930890732e+09	INFO	Starting server	{"kind": "health probe", "addr": "[::]:8081"}
1.6733800930981865e+09	INFO	Starting EventSource	{"controller": "jujucluster", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuCluster", "source": "kind source: *v1beta1.JujuCluster"}
1.6733800930982227e+09	INFO	Starting Controller	{"controller": "jujucluster", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuCluster"}
1.6733800930984538e+09	INFO	Starting EventSource	{"controller": "jujumachine", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuMachine", "source": "kind source: *v1beta1.JujuMachine"}
1.673380093098476e+09	INFO	Starting Controller	{"controller": "jujumachine", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuMachine"}
1.6733800930987048e+09	DEBUG	events	capi-juju-controller-manager-78cc545778-8m6vl_e14bf9b2-a388-43c3-9984-d441abef21b6 became leader	{"type": "Normal", "object": {"kind":"Lease","namespace":"capi-juju-system","name":"d18b2b62.cluster.x-k8s.io","uid":"52b2218f-a889-4d2e-b77a-60e20d03126a","apiVersion":"coordination.k8s.io/v1","resourceVersion":"21399"}, "reason": "LeaderElection"}
1.6733800931992176e+09	INFO	Starting workers	{"controller": "jujumachine", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuMachine", "worker count": 1}
1.6733800931992946e+09	INFO	Starting workers	{"controller": "jujucluster", "controllerGroup": "infrastructure.cluster.x-k8s.io", "controllerKind": "JujuCluster", "worker count": 1}
```

8. Create a secret containing your cloud credential data:
The cluster controller needs cloud credential information in order to create models and machines via juju. You will need to create a secret using a credential yaml file for your respective cloud. 
Below is an example yaml file for vsphere:
```yaml
# This is an example vsphere credential file
# see https://juju.is/docs/olm/add-credentials#heading--use-a-yaml-file for details regarding other clouds
credentials:
  jujucluster-sample: # cloud name
    jujucluster-sample: # credential name
      auth-type: userpass
      password: a_password
      user: a_user
      vmfolder: a_folder
```

You can create a secret containing a value key populated with the contents of the yaml file like so:

```sh
kubectl create secret generic jujucluster-sample-credential-secret --from-file=value=./your_creds.yaml -n default
```

You can name the secret whatever you want, but you will need to provide the name and namespace of the secret in the cloud portion of your cluster spec. 

```yaml
spec:
  ...
  credential:
    credentialSecretName: jujucluster-sample-credential-secret
    credentialSecretNamespace: default
```

7. Create the sample cluster resources (this sample is for vsphere clusters):
```sh
kubectl apply -f config/samples/infrastructure_v1beta1_jujucluster.yaml -n default
```

After creating the sample resources, you can check the controller-manager logs again and see that it is doing work such as bootstrapping a Juju controller and creating other resource dependencies. 
Eventually it should create a model, deploy charms, and create integrations. Once the necessary charms have come up, the cluster will be marked as ready.

### Connecting the deployed controller to a local Juju client
See the process described in the [Deploying a CNI](README.md#deploying-a-cni) for details on connecting the controller to a local client.

### Tearing things down
1.  Delete the sample cluster:
```sh
kubectl delete cluster cluster-sample
```

After the resources are deleted, the controller will clean up after itself and remove the resource dependencies it created. You should see messages about resource removal in the controller-manager-logs. It may take a while for things to get deleted and the command to finish. 

Part of this cleanup involves the removal of the juju controller namespace `controller-jujucluster-sample-k8s-cloud` and the various resources it contains, which can take several minutes. 

Before moving on, verify the Juju controller namespace was completely removed:
```sh
kubectl get ns
```
The namespace will be terminating for a while, but after several minutes should be removed the list.

2. Undeploy the controller
```sh
make undeploy
```

3. Uninstall the CRDs
```sh
make uninstall
```

The other providers can be developed in a similar way to the infrastructure provider.

### How it works
This project aims to follow the Kubernetes [Operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/)

It uses [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/) 
which provides a reconcile function responsible for synchronizing resources until the desired state is reached on the cluster 

### Modifying the API definitions
If you are editing the API definitions, generate the manifests such as CRs or CRDs using:

```sh
make manifests
```

**NOTE:** Run `make --help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)