# Registry for images
# Registry for images
REGISTRY ?= upload.rocks.canonical.com:5000
IMG_PATH ?= /cdk/capi/
IMG_NAME ?= juju-client
IMG_TAG ?= latest
# Image URL to use all building/pushing image targets (note the inclusion of upload)
IMG ?= $(REGISTRY)$(IMG_PATH)$(IMG_NAME):$(IMG_TAG)

JUJU_MAJOR ?= 3.0
JUJU_MINOR ?= 2

KUBECTL_VERSION ?= 1.26.0

.PHONY: juju ## Download juju client binary
juju:
	curl -LO https://launchpad.net/juju/${JUJU_MAJOR}/${JUJU_MAJOR}.${JUJU_MINOR}/+download/juju-${JUJU_MAJOR}.${JUJU_MINOR}-linux-amd64.tar.xz
	curl -L https://launchpad.net/juju/${JUJU_MAJOR}/${JUJU_MAJOR}.${JUJU_MINOR}/+download/juju-${JUJU_MAJOR}.${JUJU_MINOR}-linux-amd64.tar.xz/+md5 -o juju.md5
	cat juju.md5 | md5sum --check
	tar xf juju-${JUJU_MAJOR}.${JUJU_MINOR}-linux-amd64.tar.xz
	rm juju.md5
	rm juju-${JUJU_MAJOR}.${JUJU_MINOR}-linux-amd64.tar.xz
	rm juju-metadata
	curl -LO https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/amd64/kubectl
	chmod +x ./kubectl

.PHONY: docker-build
docker-build: ## Build docker image with the juju client.
	docker build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

