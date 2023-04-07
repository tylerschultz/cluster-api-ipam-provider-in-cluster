package controllers

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"go4.org/netipx"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ipamv1 "sigs.k8s.io/cluster-api/exp/ipam/api/v1alpha1"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/telekom/cluster-api-ipam-provider-in-cluster/api/v1alpha1"
	"github.com/telekom/cluster-api-ipam-provider-in-cluster/internal/poolutil"
	"github.com/telekom/cluster-api-ipam-provider-in-cluster/pkg/ipamutil"
	ipampredicates "github.com/telekom/cluster-api-ipam-provider-in-cluster/pkg/predicates"
	pooltypes "github.com/telekom/cluster-api-ipam-provider-in-cluster/pkg/types"
)

const (
	// ReleaseAddressFinalizer is used to release an IP address before cleaning up the claim.
	ReleaseAddressFinalizer = "ipam.cluster.x-k8s.io/ReleaseAddress"

	// ProtectAddressFinalizer is used to prevent deletion of an IPAddress object while its claim is not deleted.
	ProtectAddressFinalizer = "ipam.cluster.x-k8s.io/ProtectAddress"
)

// IPAddressClaimReconciler reconciles a InClusterIPPool object.
type IPAddressClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	WatchFilterValue string
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPAddressClaimReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1.IPAddressClaim{}, builder.WithPredicates(
			predicate.Or(
				ipampredicates.ClaimReferencesPoolKind(metav1.GroupKind{
					Group: v1alpha1.GroupVersion.Group,
					Kind:  "InClusterIPPool",
				}),
				ipampredicates.ClaimReferencesPoolKind(metav1.GroupKind{
					Group: v1alpha1.GroupVersion.Group,
					Kind:  "GlobalInClusterIPPool",
				}),
			),
		)).
		WithOptions(controller.Options{
			// To avoid race conditions when allocating IP Addresses, we explicitly set this to 1
			MaxConcurrentReconciles: 1,
		}).
		Owns(&ipamv1.IPAddress{}, builder.WithPredicates(
			ipampredicates.AddressReferencesPoolKind(metav1.GroupKind{
				Group: v1alpha1.GroupVersion.Group,
				Kind:  "InClusterIPPool",
			}),
		)).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Complete(r)
}

//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=inclusterippools/finalizers,verbs=update
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=globalinclusterippools/finalizers,verbs=update
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims/status;ipaddresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ipam.cluster.x-k8s.io,resources=ipaddressclaims/status;ipaddresses/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *IPAddressClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info("Reconciling claim")

	// Fetch the IPAddressClaim
	claim := &ipamv1.IPAddressClaim{}
	if err := r.Client.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(claim, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		if err := patchHelper.Patch(ctx, claim); err != nil {
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	var pool pooltypes.GenericInClusterPool

	if claim.Spec.PoolRef.Kind == "InClusterIPPool" {
		icippool := &v1alpha1.InClusterIPPool{}
		if err := r.Client.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.PoolRef.Name}, icippool); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrap(err, "failed to fetch pool")
		}
		pool = icippool
	} else if claim.Spec.PoolRef.Kind == "GlobalInClusterIPPool" {
		gicippool := &v1alpha1.GlobalInClusterIPPool{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: claim.Spec.PoolRef.Name}, gicippool); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrap(err, "failed to fetch pool")
		}
		pool = gicippool
	}

	address := &ipamv1.IPAddress{}
	if err := r.Client.Get(ctx, req.NamespacedName, address); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrap(err, "failed to fetch address")
	}

	if !controllerutil.ContainsFinalizer(claim, ReleaseAddressFinalizer) {
		controllerutil.AddFinalizer(claim, ReleaseAddressFinalizer)
	}

	if !claim.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, claim, address)
	}
	addressesInUse, err := poolutil.ListAddressesInUse(ctx, r.Client, pool.GetNamespace(), claim.Spec.PoolRef)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list addresses: %w", err)
	}

	return r.reconcile(ctx, claim, pool, addressesInUse)
}

func (r *IPAddressClaimReconciler) reconcile(ctx context.Context, claim *ipamv1.IPAddressClaim, pool pooltypes.GenericInClusterPool, addressesInUse []ipamv1.IPAddress) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if pool == nil {
		err := errors.New("pool not found")
		log.Error(err, "the referenced pool could not be found")
		return ctrl.Result{}, nil
	}

	log = log.WithValues(pool.GetObjectKind().GroupVersionKind().Kind, fmt.Sprintf("%s/%s", pool.GetNamespace(), pool.GetName()))

	poolIPSet, err := poolutil.IPPoolSpecToIPSet(pool.PoolSpec())
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to convert pool to range")
	}

	inUseIPSet, err := inUseIPSet(addressesInUse, pool.PoolSpec().Gateway, poolIPSet)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to convert IPAddressList to IPSet")
	}

	address := poolutil.AddressByName(addressesInUse, claim.Name)
	if address == nil {
		var err error
		address, err = r.allocateAddress(claim, pool, poolIPSet, inUseIPSet)
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to allocate address")
		}
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, address, func() error {
		if err := ipamutil.EnsureIPAddressOwnerReferences(r.Scheme, address, claim, pool); err != nil {
			return errors.Wrap(err, "failed to ensure owner references on address")
		}

		_ = controllerutil.AddFinalizer(address, ProtectAddressFinalizer)

		return nil
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create or patch address")
	}

	log.Info(fmt.Sprintf("IPAddress %s/%s (%s) has been %s", address.Namespace, address.Name, address.Spec.Address, operationResult),
		"IPAddressClaim", fmt.Sprintf("%s/%s", claim.Namespace, claim.Name))

	if !address.DeletionTimestamp.IsZero() {
		// We prevent deleting IPAddresses while their corresponding IPClaim still exists since we cannot guarantee that the IP
		// wil remain the same when we recreate it.
		log.Info("Address is marked for deletion, but deletion is prevented until the claim is deleted as well.", "address", address.Name)
	}

	claim.Status.AddressRef = corev1.LocalObjectReference{Name: address.Name}

	err = r.updatePoolStatus(ctx, pool, poolIPSet, inUseIPSet, log)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to patch pool status")
	}

	return ctrl.Result{}, nil
}

func (r *IPAddressClaimReconciler) updatePoolStatus(ctx context.Context, pool pooltypes.GenericInClusterPool, poolIPSet *netipx.IPSet, inUseIPSet *netipx.IPSet, log logr.Logger) error {
	poolCount := poolutil.IPSetCount(poolIPSet)
	inUseCount := poolutil.IPSetCount(inUseIPSet)
	freeCount := poolCount - inUseCount

	patchHelper, err := patch.NewHelper(pool, r.Client)
	if err != nil {
		return err
	}

	status := pool.PoolStatus()
	status.IPAddresses.Total = poolCount
	status.IPAddresses.Used = inUseCount
	status.IPAddresses.Free = freeCount

	if err := patchHelper.Patch(ctx, pool); err != nil {
		return errors.Wrap(err, "failed to patch pool")
	}

	log.Info("updated pool with usage info", "pool.Status.IPAddresses", pool.PoolStatus().IPAddresses)

	return nil
}

func (r *IPAddressClaimReconciler) reconcileDelete(ctx context.Context, claim *ipamv1.IPAddressClaim, address *ipamv1.IPAddress) (ctrl.Result, error) {
	if address.Name != "" {
		var err error
		if controllerutil.RemoveFinalizer(address, ProtectAddressFinalizer) {
			if err = r.Client.Update(ctx, address); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, errors.Wrap(err, "failed to remove address finalizer")
			}
		}

		if err == nil {
			if err := r.Client.Delete(ctx, address); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(claim, ReleaseAddressFinalizer)
	return ctrl.Result{}, nil
}

func (r *IPAddressClaimReconciler) allocateAddress(claim *ipamv1.IPAddressClaim, pool pooltypes.GenericInClusterPool, poolIPSet *netipx.IPSet, inUseIPSet *netipx.IPSet) (*ipamv1.IPAddress, error) {
	poolSpec := pool.PoolSpec()

	freeIP, err := poolutil.FindFreeAddress(poolIPSet, inUseIPSet)
	if err != nil {
		return nil, fmt.Errorf("failed to find free address: %w", err)
	}

	address := ipamutil.NewIPAddress(claim, pool)
	address.Spec.Address = freeIP.String()
	address.Spec.Gateway = poolSpec.Gateway
	address.Spec.Prefix = poolSpec.Prefix

	return &address, nil
}

func inUseIPSet(inUseIPAddresses []ipamv1.IPAddress, gatewayStr string, poolIPSet *netipx.IPSet) (*netipx.IPSet, error) {
	builder := &netipx.IPSetBuilder{}

	for _, inUseIPAddress := range inUseIPAddresses {
		inUseAddr, err := netip.ParseAddr(inUseIPAddress.Spec.Address)
		if err != nil {
			return nil, err
		}
		builder.Add(inUseAddr)
	}

	if gatewayStr != "" {
		gatewayAddr, err := netip.ParseAddr(gatewayStr)
		if err != nil {
			return nil, err
		}

		if poolIPSet.Contains(gatewayAddr) {
			builder.Add(gatewayAddr)
		}
	}

	return builder.IPSet()
}
