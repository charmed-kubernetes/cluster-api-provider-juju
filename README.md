# cluster-api-provider-juju

A Cluster API infrastructure provider which uses [Juju][].

## Description

This Cluster API infrastructure provider manages a Juju model containing [Charmed Kubernetes][] components.
This provider is used alongside the [Charmed Kubernetes Control Plane Provider][] and the [Charmed Kubernetes Bootstrap Provider][]. These providers support deploying Charmed Kubernetes 1.27+. It does not work with other Cluster API providers.

## Getting Started

### Creating the management cluster

You’ll need a Kubernetes cluster to host the various controllers and resources. Charmed Kubernetes can be used for this purpose if you wish. Assuming you have a Juju controller, cloud, and cloud credentials set up already, you can add a new model and deploy Kubernetes-Core. Note: Your cluster will need storage and load-balancing capabilties, so ensure your overlay contains the cloud provider/integrator required to set that up. For more details on installing Charmed Kubernetes, see the [Charmed Kubernetes documentation][]

```sh
juju deploy kubernetes-core --overlay your-overlay.yaml --trust --channel 1.27/stable
```

After the deployment is active, copy the kubeconfig file to your local machine:

```sh
juju ssh kubernetes-control-plane/leader -- cat config > ~/.kube/config
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

Install the ClusterAPI command-line tool clusterctl using [these instructions][].

Create the [clusterctl configuration file][] and add the following:

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
Generate a cluster from the provided template. The template provided is for vSphere, but modifications can be made to the generated YAML for other clouds.

```sh
# review list of variables needed for the cluster template
clusterctl generate cluster jujucluster --from ./templates/cluster-template-vsphere.yaml --list-variables

# set environment variables (edit the file as needed before sourcing it)
source ./templates/cluster-template-vsphere.rc

# generate cluster
clusterctl generate cluster jujucluster --from ./templates/cluster-template-vsphere.yaml > jujucluster.yaml
```

If you have any configuration changes to make to the generated yaml (model config, charm configuration, etc) spec, you can do so at this point. 

The cluster controller needs cloud credential information in order to create models and machines via Juju. You will need to create a secret using a credential yaml file for your respective cloud. 
Below is an example YAML file for vSphere. See the [Juju documentation][] for details regarding other clouds

```yaml
# This is an example vSphere credential file
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

You can then deploy the cluster using the generated YAML file:

```sh
kubectl apply -f jujucluster.yaml
```

The Charmed Kubernetes cluster will deployed. It may take some time for the machines to come up and for the charms to be fully deployed. You can check their current status using the following command:

```sh
kubectl get machines
```

You can also check the jujucluster status, which will contain the status of the Juju model:

```sh
kubectl describe jujucluster jujucluster
```

Once all machines are in the running phase, provisioning will be complete. Note that at this point your control plane will not be ready, as a CNI will need to be deployed. 

### Connecting the cluster to a local Juju client

Some tasks (such as deployment of CNI charms or general debugging) may require a local Juju client be connected to the Juju controller that is deployed with the cluster. Doing so will require you run run the `juju register` command, and pass it a registration string. 

The cluster controller creates a secret containing a Juju registration string that you can use to connect your local Juju client. Assuming your cluster is in the default namespace, you can list all secrets:

```sh
kubectl get secrets
```

The registration secret is named `<your-cluster-name>-registration-string`. 
The encoded registration data is contained in the key `registration-string`. You can decode the data contained in that key with the following command:

```sh
REGISTRATION_STRING=kubectl get secret jujucluster-registration-string -o jsonpath='{.data.registration-string}' | base64 --decode
```

The above command will output the decoded registration string, which you can then pass to the `juju register` command:

```sh
juju register $REGISTRATION_STRING
```

You will be prompted to enter a new password for the account, and choose a controller name (accepting the default name is fine). You can then switch 
to the model for your cluster. 

```sh
juju switch admin/jujucluster
```

This account does not have write access to the model, but it can be helpful for debugging. 

If you would like to perform operations which require write access, you will need to login to the admin account. Admin account credentials are contained in a secret named `<your-cluster-name>-juju-controller-data`. You can obtain the admin password by decoding the admin controller data with the following command:

```sh
kubectl get secret jujucluster-juju-controller-data -o jsonpath='{.data.controller-data}' | base64 --decode
```

Log in to the admin account using the obtained password:

```sh
juju logout -c jujucluster-k8s-cloud
juju login -u admin -c jujucluster-k8s-cloud
```

You will then be prompted for the admin password from the `<your-cluster-name>-juju-controller-data` secret. After entering the password, you can switch to the model:

```sh
juju switch jujucluster
``` 

### Deploying a CNI

Currently, Juju client interaction is necessary to deploy a CNI charm. This aspect of the provider will be improved in the future. 

You can deploy and integrate the Calico charm using the following commands:

```sh
juju deploy calico --channel=1.27/stable --config vxlan=Always -m jujucluster
juju integrate calico:etcd etcd:db
juju integrate calico:cni kubernetes-control-plane:cni
juju integrate calico:cni kubernetes-worker:cni
```

Note that if you are using a version of Juju prior to 3.0, you will need to use the `relate` command instead of `integrate`. 

Verify all applications in the model reach an active/idle status (this might take a few minutes after deploying the CNI charm):

```sh
juju status
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

## Contributing
Please see the [contributing guide](CONTRIBUTING.md) for information on facilitating local development of the providers.

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

<!-- LINKS -->
[Juju]: https://juju.is/
[Charmed Kubernetes]: https://ubuntu.com/kubernetes/charmed-k8s
[Charmed Kubernetes Control Plane Provider]: https://github.com/charmed-kubernetes/cluster-api-control-plane-provider-charmed-k8s
[Charmed Kubernetes Bootstrap Provider]: https://github.com/charmed-kubernetes/cluster-api-bootstrap-provider-charmed-k8s
[these instructions]: https://cluster-api.sigs.k8s.io/user/quick-start.html#install-clusterctl
[Juju documentation]: https://juju.is/docs/olm/add-credentials#heading--use-a-yaml-file
[clusterctl configuration file]: https://cluster-api.sigs.k8s.io/clusterctl/configuration.html
[Charmed Kubernetes documentation]: https://ubuntu.com/kubernetes/docs/install-manual
