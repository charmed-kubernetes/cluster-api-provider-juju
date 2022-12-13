# Juju Client

This directory contains a Makefile for building and pushing Docker images containing a Juju client binary and a kubectl binary. 

The Juju client binary is used to bootstrap Juju controller into the k8s-cluster. The kubectl binary is used to generate a kubeconfig file that can then be consumed by the Juju client as part of the add-k8s/bootstrap process.

# Building and Pushing the Image
The Make file contains some variables that determine what versions of the Juju client and kubectl are used, as well as some variables that determine the registry used and the path the image is published too. 

Once those are set to your liking, you can build and push the image like so:

```
# Download the juju client and kubectl
make juju
make docker-build docker-push
```

# Manual Testing

The directory contains a `juju-pod.yaml` file that can be used for manually testing the creation of a juju controller. The pod spec contains the commands needed to create the kubeconfig as well as the add-k8s and bootstrap commands that ultimately create a controller pod in the cluster. Note that the juju client requires many RBAC permssions in order to create the controller-related resources, so you will need to ensure you have a cluster-admin ServiceAccount to use with the pod before applying the yaml:

```
kubectl create ns juju-client
kubectl create serviceaccount juju-client -n juju-client
kubectl create clusterrolebinding juju-client --clusterrole=cluster-admin --serviceaccount=juju-client:juju-client
kubectl apply -f juju-pod.yaml 
```
The juju client commands run by the pod should eventually result in a namespace called controller-my-k8s-cloud being created, with a controller pod running in that namespace.

After a few minutes, you should see a controller pod running:
```
kubectl get pod -n controller-my-k8s-cloud
```
