# cluster-api-provider-juju
A Cluster API infrastructure provider that utilizes [Juju](https://juju.is/).

## Description
This Cluster API infrastructure provider manages a Juju model containing [Charmed Kubernetes](https://ubuntu.com/kubernetes/charmed-k8s) components.
This provider is used alongside the [Charmed Kubernetes Control Plane Provider](https://github.com/charmed-kubernetes/cluster-api-control-plane-provider-charmed-k8s)
and the [Charmed Kubernetes Bootstrap Provider](https://github.com/charmed-kubernetes/cluster-api-bootstrap-provider-charmed-k8s). It does not work with other Cluster API providers. These providers support deploying Charmed Kubernetes 1.27+

## Getting Started

### Creating the management cluster
You’ll need a Kubernetes cluster to host the various controllers and resources. Charmed Kubernetes can be used for this purpose if you wish. Assuming you have a juju controller, cloud, and cloud credentials set up already, you can add a new model and deploy Kubernetes-Core. Note: Your cluster will need storage and load-balancing capabilties, so ensure your overlay contains the cloud provider/integrator required to set that up. 
```sh
juju deploy kubernetes-core --overlay your-overlay.yaml --trust --channel 1.27/stable
```

After the deployment is active, copy the kubeconfig to your local machine:
```sh
juju scp kubernetes-control-plane/0:config ~/.kube/config
```

You may need to create a storage class if your cluster does not have one created by default:
```sh
kubectl apply -f my-storageclass.yaml
```

For example, the YAML file for a vsphere storage class might look like this:
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: mystorage
provisioner: kubernetes.io/vsphere-volume
parameters:
  diskformat: zeroedthick
```

### Installing ClusterAPI
Install the ClusterAPI command-line tool clusterctl using [these instructions](https://cluster-api.sigs.k8s.io/user/quick-start.html#install-clusterctl).

Create the [clusterctl configuration file](https://cluster-api.sigs.k8s.io/clusterctl/configuration.html) and add the following:
```yaml
providers:
- name: "juju"
  url: "https://github.com/charmed-kubernetes/cluster-api-provider-juju/releases/latest/infrastructure-components.yaml"
  type: "InfrastructureProvider"
- name: "charmed-k8s"
  url: "https://github.com/charmed-kubernetes/cluster-api-bootstrap-provider-charmed-k8s/releases/latest/bootstrap-components.yaml"
  type: "BootstrapProvider"
- name: "charmed-k8s"
  url: "https://github.com/charmed-kubernetes/cluster-api-control-plane-provider-charmed-k8s/releases/latest/control-plane-components.yaml"
  type: "ControlPlaneProvider"
```

Run the init command to install the ClusterAPI providers:
```sh
clusterctl init -i juju:v0.1.0 -b charmed-k8s:v0.1.0 -c charmed-k8s:v0.1.0
```

Check the status to make sure things are running:
```sh
kubectl describe -n capi-juju-system pod | grep -A 5 Conditions
kubectl describe -n capi-charmed-k8s-control-plane-system pod | grep -A 5 Conditions
kubectl describe -n capi-charmed-k8s-bootstrap-system pod | grep -A 5 Conditions
```

### Creating a workload cluster
Generate a cluster from the provided template. The template provided is for vsphere, but modifications can be made to the generated yaml for other clouds.
```sh
# review list of variables needed for the cluster template
clusterctl generate cluster jujucluster --from ./templates/cluster-template-vsphere.yaml --list-variables

# set environment variables (edit the file as needed before sourcing it)
source ./templates/cluster-template-vsphere.rc

# generate cluster
clusterctl generate cluster jujucluster --from ./templates/cluster-template-vsphere.yaml > jujucluster.yaml
```

If you have any configuration changes to make to the cluster (model config, charm configuration, etc) spec, you can do so at this point. 

The cluster controller needs cloud credential information in order to create models and machines via Juju. You will need to create a secret using a credential yaml file for your respective cloud. 
Below is an example yaml file for vsphere. See the [Juju documentation](https://juju.is/docs/olm/add-credentials#heading--use-a-yaml-file) for details regarding other clouds
```yaml
# This is an example vsphere credential file
# see https://juju.is/docs/olm/add-credentials#heading--use-a-yaml-file for details regarding other clouds
credentials:
  jujucluster: # cloud name
    jujucluster: # credential name
      auth-type: userpass
      password: a_password
      user: a_user
      vmfolder: a_folder
```

Note that the cloud and credential name should match the name you gave your cluster. 

You can create a secret containing a value key populated with the contents of the yaml file like so:

```sh
kubectl create secret generic jujucluster-credential-secret --from-file=value=./your_creds.yaml -n default
```

You can then deploy the cluster using the generated yaml file:
```sh
kubectl apply -f jujucluster.yaml
```

The Charmed Kubernetes cluster will deployed. It may take some time for the machines to come up and for the charms to be fully deployed. You can check the status of them using the following command:
```sh
kubectl get machines
```

You can also check the cluster status, which will contain the status of the Juju model:
```sh
kubectl describe cluster jujucluster
```

Once all machines are in the running phase, provisioning will be complete. Note that at this point your control plane will not be ready, as a CNI will need to be deployed. 

### Deploying a CNI

Currently, juju client interaction is necessary to deploy a CNI. This aspect of the provider will be improved in the future, but for now, you can add the deployed cluster to a local juju client using the cluster's registration string. 

The cluster controller creates a secret containing a Juju registration string that you can use to connect your local Juju client. Assuming your cluster is in the default namespace, you can list all secrets:
```sh
kubectl get secrets
```

The registration secret is named `<your-cluster-name>-registration-string`. 
The encoded registration data is contained in the key `registration-string`. You can decode the data contained in that key with the following command:
```sh
kubectl get secret jujucluster-registration-secret -o jsonpath='{.data.registration-string}' | base64 --decode
```

The above command will output the decoded registration string, which you can then pass to the `juju register` command:
```sh
juju register <registration_string>
```

You will be prompted to enter a new password for the account, and choose a controller name (accepting the default name is fine). You can then switch 
to the model for your cluster. The account does not have write access to the model, but it can be helpful for debugging. 

Now that you've got the controller added to your client, you will need to login to the admin account. You can follow a similar secret decoding 
process described above to get the admin account data, which will contain the admin password.
```sh
kubectl get secret jujucluster-juju-controller-data -o jsonpath='{.data.controller-data}' | base64 --decode
```

Login to the admin account using the obtained password
```sh
juju logout 
juju login -u admin -c jujucluster-k8s-cloud
```

You will then be prompted for the admin password from the `<your-cluster-name>-juju-controller-data` secret. 

Once logged in, you can deploy a CNI such as Calico. 

```sh
juju deploy calico --channel=1.27/stable --config vxlan=Always
juju relate calico:etcd etcd:db;juju relate calico:cni kubernetes-control-plane:cni;juju relate calico:cni kubernetes-worker:cni
```

Once the CNI is up, your control plane should now be ready, which you can verify using:
```sh
kubectl describe charmedk8scontrolplane
```

### Obtaining a kubeconfig file
You can obtain the kubeconfig for your workload cluster with the following command:
```sh
clusterctl get kubeconfig jujucluster > kubeconfig
```
and then use it to access the cluster:
```sh
KUBECONFIG=./kubeconfig kubectl get nodes
```
### Cluster removal
To remove your workload cluster, you can delete the cluster object:
```sh
kubectl delete cluster jujucluster
```

The removal process can take some time. 

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
See the process described in the [Deploying a CNI](#deploying-a-cni) for details on connecting the controller to a local client.

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

## License

Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

