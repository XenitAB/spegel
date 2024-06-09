# FAQ

Frequently asked questions, please read these before creating a new issue.

## Can I use Spegel in production?

Spegel is being used by multiple users in production for over a year without any major issues. The great thing is that pulling images would not stop working if you for some reason would find an issue with Spegel.
A fallback to the original registry will always occur if Spegel can't be reached or serve the requested image.

## What performance increase can I expect with Spegel?

Read the [benchmark documentation](./BENCHMARK.md) for information of expected gains.

## How do I know that Spegel is working? 

Spegel is meant to be a painless experience to install. Image pulls will fallback to the original registry if Spegel does not work, meaning that it can be difficult to determine if things are working or not. Spegel has a UI that visualizes incoming and outgoing requests, this will allow you understand if images are pulled from other Spegel instances or not.

The UI is disabled by default as it adds additional overhead. To access the UI enable the feature in the Helm values.

```yaml
spegel:
  visualize:
    enabled: true
```

After all Spegel instances have restarted you can port forward to one of the Spegel pods.

```shell
kubectl -n spegel port-forward ${POD_NAME} 9090
```

Open the UI at `http://localhost:9090/visualize` in a browser. If all is configured propery you should be presented with and interface showing registry requests.

## Will image pulls break or be delayed if a spegel instance fails or is removed?

Spegel acts as a best-effort cache and the worst-case scenario is always that images are pulled from the upstream registry (e.g. Docker Hub).

However, should a spegel instance fail (perhaps because the node died), there will be a time interval when its images remain advertised. Currently, spegel advertises images with a TTL of 10 minutes. Other spegel peers may try to forward requests to the failed instance, delaying the response to the pulling client. In benign scenarios, this delay is the length of an intra-cluster round trip (the HTTP request and an ICMP unreachable response), likely <1ms. Of course, there are less benign scenarios (e.g. inter-node packet loss) where no replies will come back and spegel's forwarder will eventually time out before moving on to the next available peer. Spegel uses the standard library's httputil.ReverseProxy to forward requests, which in turn depends on DefaultTransport to decide how long to wait before giving up.

Please note that a client is likely to request several layers in parallel and in many cases the advertising instances will have a similar routing distance, so spegel will spread its forwards across those instances. Thus, the benign scenario is unlikely to impact pod startup time. Only when the routing distance is different (e.g. edge locations) or when an image dominated by one large layer is affected is pod startup time materially increased.

## Why am I not able to pull the new version of my tagged image?

Reusing the same tag multiple times for different versions of an image is generally a bad idea. The most common scenario is the use of the `latest` tag. This makes it difficult to determine which version of the image is being used. On top of that, the image will not be updated if it is already cached on the node.
Some people have chosen to power forward with reusing tags and chosen to instead set the image pull policy to `AlwaysPull`, forcing the image to be updated every time a pod is started. This will however not work with Spegel as the tag could be resolved by another node in the cluster resulting in the same "old" image being pulled.
There are two solutions to work around this problem, allowing users to continue with their way of working before using Spegel.

The best and preferable solution is to deploy [k8s-digester](https://github.com/google/k8s-digester) alongside Spegel. This will allow you to enjoy all the benefits of Spegel will continuously updating image tag versions. The way it works is that k8s-digester will, for each pod created, resolve tags to image digests and add them to the image reference.
This means that all pods that originally reference images by tag will instead do so with digest. This means that k8s-digester will resolve the new digest for a tag if a new version is pushed to the registry. Using k8s-digester means that tags will be updated while using Spegel to distribute the layers between nodes. It also means that Spegel will be able
to continue distributing images if the external registry became unavailable. The reason this works is that the mutating webhook is configured to ignore errors, and instead, Spegel will be used to resolve the tag to a digest.

One caveat when deploying k8s-digester is that it will by default modify both pods but also any other parent resource that creates pods. This in turn has the side effect of only setting the
digest once when the parent resource is created, and never again. For that reason it is a good idea to modify the mutating webhook to only include pods, that way the digest will be
updated every time a new pod is created.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: digester-mutating-webhook-configuration
  labels:
    control-plane: controller-manager
    digester/operation: webhook
    digester/system: "yes"
webhooks:
- name: digester-webhook-service.digester-system.svc
  admissionReviewVersions:
  - v1
  - v1beta1
  clientConfig:
    service:
      name: digester-webhook-service
      namespace: digester-system
      path: /v1/mutate
    caBundle: Cg==
  failurePolicy: Ignore # kpt-set: ${failure-policy}
  namespaceSelector:
    matchLabels:
      digest-resolution: enabled
  reinvocationPolicy: IfNeeded
  rules:
  - resources:
    - pods
    apiGroups:
    - ''
    apiVersions:
    - v1
    operations:
    - CREATE
    - UPDATE
    scope: Namespaced
  sideEffects: None
  timeoutSeconds: 15
```

The second option, which should be used only if using k8s-digester is not possible, is to disable tag resolving altogether in Spegel. There are two options when doing this. It can either be disabled only for `latest` tags or for all tags. 

This can be done by changing the Helm charts values from their defaults.

```yaml
spegel:
  resolveTags: false
  resolveLatestTag: false
```

Please note that this does however remove Spegel's ability to protect against registry outages for any images referenced by tags.

## Why am I able to pull private images without image pull secrets?

An image pulled by a Kubernetes node is cached locally on disk. Meaning that other pods running on the same node that require the same image do not have to pull the same image again. Spegel relies on this mechanism to be able to distribute images.
This may however not be a desirable feature when running a multi-tenant cluster where private images are pulled using credentials. In this scenario, only those pods with the correct credentials would be able to use the image.
Ownership of private images has been an issue for a long time in Kubernetes as indicated by the unresolved issue https://github.com/kubernetes/kubernetes/issues/18787 created back in 2015. The short answer is that a good solution does not exist, with or without Spegel.
The current [suggested solution](https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/#alwayspullimages) is to enforce an `AlwaysPull` image policy for private images that require authentication. Doing so will force a request to the registry to
validate the digest or resolve the tag. This request will only succeed with the proper authentication. This is a mediocre solution at best as it creates a hard dependency on the external registry, meaning the pod will not be able to start even if the image is cached on the node.

This solution does however not work when using Spegel, instead, Spegel may make the problem worse. Without Spegel an image that would want to use a private image, it does not have access to would have to be scheduled on a node that has already pulled the image.
With Spegel that image will be available to all nodes in the cluster. Currently, a good solution for Spegel does not exist. There are two reasons for this. The first is that credentials are not included when pulling an image from a registry mirror, a good choice as doing so would mean sharing credentials with third parties.
Additionally, Spegel would have no method of validating the credentials even if they were included in the requests. So for the time being if you have these types of requirements Spegel may not be the choice for you.

## How do I use Spegel in conjunction with another registry cache?

Spegel can be used with other registry caches in cases where the best effort caching offered by Spegel is not enough. In these situations, if the image is not cached within the cluster the image should be pulled from the secondary cache.
This is configured by adding the domain of the registry to the `additionalMirrorRegistries` list in the Helm values. Registries added to this list will be included in the mirror configuration created by Spegel.

```yaml
spegel:
  additionalMirrorRegistries:
    - https://zot.example.com
```
