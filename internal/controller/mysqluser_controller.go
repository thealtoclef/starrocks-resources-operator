/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	_ "github.com/go-sql-driver/mysql"

	mysqlv1alpha1 "github.com/nakamasato/mysql-operator/api/v1alpha1"
	"github.com/nakamasato/mysql-operator/internal/metrics"
	mysqlinternal "github.com/nakamasato/mysql-operator/internal/mysql"
)

const (
	mysqlUserFinalizer                         = "mysqluser.nakamasato.com/finalizer"
	mysqlUserReasonCompleted                   = "User are successfully reconciled"
	mysqlUserReasonMySQLConnectionFailed       = "Failed to connect to cluster"
	mysqlUserReasonMySQLFailedToCreateUser     = "Failed to create user"
	mysqlUserReasonMySQLFailedToUpdatePassword = "Failed to update password"
	mysqlUserReasonMySQLFailedToGetSecret      = "Failed to get Secret"
	mysqlUserReasonMYSQLFailedToGrant          = "Failed to grant"
	mysqlUserReasonMySQLFetchFailed            = "Failed to fetch cluster"
	mysqlUserPhaseReady                        = "Ready"
	mysqlUserPhaseNotReady                     = "NotReady"
)

// MySQLUserReconciler reconciles a MySQLUser object
type MySQLUserReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	MySQLClients mysqlinternal.MySQLClients
}

//+kubebuilder:rbac:groups=mysql.nakamasato.com,resources=mysqlusers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=mysql.nakamasato.com,resources=mysqlusers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=mysql.nakamasato.com,resources=mysqlusers/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;update;patch

// Reconcile function is responsible for managing MySQLUser.
// Create MySQL user if not exist in the target MySQL, and drop it
// if the corresponding object is deleted.
func (r *MySQLUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx).WithName("MySQLUserReconciler")

	// Fetch MySQLUser
	mysqlUser := &mysqlv1alpha1.MySQLUser{}
	err := r.Get(ctx, req.NamespacedName, mysqlUser)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Info("[FetchMySQLUser] Not found", "req.NamespacedName", req.NamespacedName)
			return ctrl.Result{}, nil
		}

		log.Error(err, "[FetchMySQLUser] Failed")
		return ctrl.Result{}, err
	}
	log.Info("[FetchMySQLUser] Found.", "name", mysqlUser.Name, "mysqlUser.Namespace", mysqlUser.Namespace)
	clusterName := mysqlUser.Spec.ClusterName
	userIdentity := mysqlUser.GetUserIdentity()
	secretRef := mysqlUser.Spec.SecretRef
	grants := mysqlUser.Spec.Grants

	// Fetch MySQL
	mysql := &mysqlv1alpha1.MySQL{}
	var mysqlNamespacedName = client.ObjectKey{Namespace: req.Namespace, Name: clusterName}
	if err := r.Get(ctx, mysqlNamespacedName, mysql); err != nil {
		log.Error(err, "[FetchMySQL] Failed")
		mysqlUser.Status.Phase = mysqlUserPhaseNotReady
		mysqlUser.Status.Reason = mysqlUserReasonMySQLFetchFailed
		if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
			log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
			return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("[FetchMySQL] Found")

	// SetOwnerReference if not exists
	if !r.ifOwnerReferencesContains(mysqlUser.OwnerReferences, mysql) {
		err := controllerutil.SetControllerReference(mysql, mysqlUser, r.Scheme)
		if err != nil {
			return ctrl.Result{}, err //requeue
		}
		err = r.Update(ctx, mysqlUser)
		if err != nil {
			return ctrl.Result{}, err //requeue
		}
	}

	// Get MySQL client
	mysqlClient, err := r.MySQLClients.GetClient(mysql.GetKey())
	if err != nil {
		mysqlUser.Status.Phase = mysqlUserPhaseNotReady
		mysqlUser.Status.Reason = mysqlUserReasonMySQLConnectionFailed
		log.Error(err, "[MySQLClient] Failed to connect to cluster", "key", mysql.GetKey(), "clusterName", clusterName)
		if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
			log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
			return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
		}
		return ctrl.Result{}, err //requeue
	}
	log.Info("[MySQLClient] Successfully connected")

	// Finalize if DeletionTimestamp exists
	if !mysqlUser.GetDeletionTimestamp().IsZero() {
		log.Info("isMysqlUserMarkedToBeDeleted is true")
		if controllerutil.ContainsFinalizer(mysqlUser, mysqlUserFinalizer) {
			log.Info("ContainsFinalizer is true")
			// Run finalization logic for mysqlUserFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.finalizeMySQLUser(ctx, mysqlClient, mysqlUser); err != nil {
				log.Error(err, "Failed to complete finalizeMySQLUser")
				return ctrl.Result{}, err
			}
			log.Info("finalizeMySQLUser completed")
			// Remove mysqlUserFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			log.Info("removing finalizer")
			if controllerutil.RemoveFinalizer(mysqlUser, mysqlUserFinalizer) {
				log.Info("RemoveFinalizer completed")
				err := r.Update(ctx, mysqlUser)
				log.Info("Update")
				if err != nil {
					log.Error(err, "Failed to update mysqlUser")
					return ctrl.Result{}, err
				}
				log.Info("Update completed")
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, nil // should return success when not having the finalizer
	}

	// Add finalizer for this CR
	log.Info("Add Finalizer for this CR")
	if controllerutil.AddFinalizer(mysqlUser, mysqlUserFinalizer) {
		log.Info("Added Finalizer")
		err = r.Update(ctx, mysqlUser)
		if err != nil {
			log.Info("Failed to update after adding finalizer")
			return ctrl.Result{}, err // requeue
		}
		log.Info("Updated successfully after adding finalizer")
	} else {
		log.Info("already has finalizer")
	}

	// Skip all the following steps if MySQL is being Deleted
	if !mysql.GetDeletionTimestamp().IsZero() {
		log.Info("MySQL is being deleted. MySQLUser cannot be created.", "mysql", mysql.Name, "mysqlUser", mysqlUser.Name)
		return ctrl.Result{}, err
	}

	// Get password from Secret
	secret := &v1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: secretRef.Name}, secret)
	if err != nil {
		log.Error(err, "[password] Failed to get Secret", "secretRef", secretRef)
		mysqlUser.Status.Phase = mysqlUserPhaseNotReady
		mysqlUser.Status.Reason = mysqlUserReasonMySQLFailedToGetSecret
		if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
			log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
			return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info("[password] Get password from Secret", "secretRef", secretRef)
	password := string(secret.Data[secretRef.Key])

	// Check if MySQL user exists
	_, err = mysqlClient.ExecContext(ctx, fmt.Sprintf("SHOW GRANTS FOR %s", userIdentity))
	if err != nil {
		// Create User if not exists with the password set above.
		_, err = mysqlClient.ExecContext(ctx,
			fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED BY '%s'", userIdentity, password))
		if err != nil {
			log.Error(err, "[MySQL] Failed to create User", "clusterName", clusterName, "userIdentity", userIdentity)
			mysqlUser.Status.Phase = mysqlUserPhaseNotReady
			mysqlUser.Status.Reason = mysqlUserReasonMySQLFailedToCreateUser
			if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
				log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
				return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
			}
			return ctrl.Result{}, err //requeue
		}
		log.Info("[MySQL] Created User", "clusterName", clusterName, "userIdentity", userIdentity)
		mysqlUser.Status.UserCreated = true
		metrics.MysqlUserCreatedTotal.Increment()
	} else {
		mysqlUser.Status.UserCreated = true
		// Update password of User if already exists with the password set above.
		_, err = mysqlClient.ExecContext(ctx,
			fmt.Sprintf("ALTER USER %s IDENTIFIED BY '%s'", userIdentity, password))
		if err != nil {
			log.Error(err, "[MySQL] Failed to update password of User", "clusterName", clusterName, "userIdentity", userIdentity)
			mysqlUser.Status.Phase = mysqlUserPhaseNotReady
			mysqlUser.Status.Reason = mysqlUserReasonMySQLFailedToUpdatePassword
			if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
				log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
				return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
			}
			return ctrl.Result{}, err //requeue
		}
		log.Info("[MySQL] Updated password of User", "clusterName", clusterName, "userIdentity", userIdentity)
	}

	// Update Grants
	err = r.updateGrants(ctx, mysqlClient, userIdentity, grants)
	if err != nil {
		log.Error(err, "[MySQL] Failed to update Grants", "clusterName", clusterName, "userIdentity", userIdentity)
		mysqlUser.Status.Phase = mysqlUserPhaseNotReady
		mysqlUser.Status.Reason = mysqlUserReasonMYSQLFailedToGrant
		if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
			log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
			return ctrl.Result{RequeueAfter: time.Second}, nil // requeue after 1 second
		}
		return ctrl.Result{}, err
	}
	// Update phase and reason of MySQLUser status to Ready and Completed
	mysqlUser.Status.Phase = mysqlUserPhaseReady
	mysqlUser.Status.Reason = mysqlUserReasonCompleted
	if serr := r.Status().Update(ctx, mysqlUser); serr != nil {
		log.Error(serr, "Failed to update MySQLUser status", "mysqlUser", mysqlUser.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MySQLUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&mysqlv1alpha1.MySQLUser{}).
		Complete(r)
}

// finalizeMySQLUser drops MySQL user
func (r *MySQLUserReconciler) finalizeMySQLUser(ctx context.Context, mysqlClient *sql.DB, mysqlUser *mysqlv1alpha1.MySQLUser) error {
	if mysqlUser.Status.UserCreated {
		_, err := mysqlClient.ExecContext(ctx, fmt.Sprintf("DROP USER IF EXISTS '%s'@'%s'", mysqlUser.Spec.Username, mysqlUser.Spec.Host))
		if err != nil {
			return err
		}
		metrics.MysqlUserDeletedTotal.Increment()
	}

	return nil
}

func (r *MySQLUserReconciler) ifOwnerReferencesContains(ownerReferences []metav1.OwnerReference, mysql *mysqlv1alpha1.MySQL) bool {
	for _, ref := range ownerReferences {
		if ref.APIVersion == "mysql.nakamasato.com/v1alpha1" && ref.Kind == "MySQL" && ref.UID == mysql.UID {
			return true
		}
	}
	return false
}

type Grant struct {
	UserIdentity       sql.NullString
	Comment            sql.NullString
	Password           sql.NullString
	Roles              sql.NullString
	GlobalPrivs        sql.NullString
	CatalogPrivs       sql.NullString
	DatabasePrivs      sql.NullString
	TablePrivs         sql.NullString
	ColPrivs           sql.NullString
	ResourcePrivs      sql.NullString
	CloudClusterPrivs  sql.NullString
	CloudStagePrivs    sql.NullString
	StorageVaultPrivs  sql.NullString
	WorkloadGroupPrivs sql.NullString
	ComputeGroupPrivs  sql.NullString
}

type EntityType string

const (
	Table         EntityType = "table"
	Resource      EntityType = "resource"
	WorkloadGroup EntityType = "workload_group"
)

func (t EntityType) Equals(other EntityType) bool {
	return t == other
}

type Entity struct {
	Type EntityType
	Name string
}

func (e Entity) IDString() string {
	return fmt.Sprintf("%s:%s", e.Type, e.Name)
}

func (e Entity) SQLString() string {
	switch e.Type {
	case Resource:
		return fmt.Sprintf("RESOURCE '%s'", e.Name)
	case WorkloadGroup:
		return fmt.Sprintf("WORKLOAD GROUP '%s'", e.Name)
	default:
		return e.Name
	}
}

func (e Entity) Equals(other Entity) bool {
	return e.Type == other.Type && e.Name == other.Name
}

func normalizeColumnOrder(perm string) string {
	re := regexp.MustCompile(`^([^(]*)\((.*)\)$`)
	// We may get inputs like
	// 	SELECT(b,a,c)   -> SELECT(a,b,c)
	// 	DELETE          -> DELETE
	//  SELECT (a,b,c)  -> SELECT(a,b,c)
	// if it's without parentheses, return it right away.
	// Else split what is inside, sort it, concat together and return the result.
	m := re.FindStringSubmatch(perm)
	if m == nil || len(m) < 3 {
		return perm
	}

	parts := strings.Split(m[2], ",")
	for i := range parts {
		parts[i] = strings.Trim(parts[i], "` ")
	}
	sort.Strings(parts)
	precursor := strings.Trim(m[1], " ")
	partsTogether := strings.Join(parts, ", ")
	return fmt.Sprintf("%s(%s)", precursor, partsTogether)
}

func normalizePerms(perms []string) []string {
	ret := []string{}
	for _, perm := range perms {
		// Remove leading and trailing backticks and spaces
		permNorm := strings.Trim(perm, "` ")
		permUcase := strings.ToUpper(permNorm)

		permSortedColumns := normalizeColumnOrder(permUcase)

		ret = append(ret, permSortedColumns)
	}

	// Sort permissions
	sort.Strings(ret)

	return ret
}

func buildGrants(privs sql.NullString, entityType EntityType) ([]mysqlv1alpha1.Grant, error) {
	var grants []mysqlv1alpha1.Grant
	if privs.Valid && privs.String != "" {
		entries := strings.Split(privs.String, ";")
		for _, entry := range entries {
			var entity Entity = Entity{Type: entityType}
			var privileges string
			entryParts := strings.Split(entry, ":")
			if len(entryParts) == 2 {
				entity.Name = strings.TrimSpace(entryParts[0])
				privileges = strings.TrimSpace(entryParts[1])
			} else if len(entryParts) == 1 {
				// If no target is specified, use global (*.*.*)
				entity.Name = "*.*.*"
				privileges = strings.TrimSpace(entryParts[0])
			} else {
				return nil, fmt.Errorf("invalid privilege format: %s", entry)
			}

			// Ensure that entity.Name matches the format *.*.* for the Table entity type
			if entityType == Table {
				nameParts := strings.Split(strings.TrimSpace(entity.Name), ".")
				for len(nameParts) < 3 {
					nameParts = append(nameParts, "*")
				}
				entity.Name = strings.Join(nameParts, ".")
			}

			grants = append(grants, mysqlv1alpha1.Grant{
				Privileges: normalizePerms(strings.Split(privileges, ",")),
				Target:     entity.SQLString(),
			})
		}
	}
	return grants, nil
}

func fetchExistingGrants(ctx context.Context, mysqlClient *sql.DB, userIdentity string) ([]mysqlv1alpha1.Grant, error) {
	var grants []mysqlv1alpha1.Grant

	log := log.FromContext(ctx)
	rows, err := mysqlClient.QueryContext(ctx, fmt.Sprintf("SHOW GRANTS FOR %s;", userIdentity))
	if err != nil {
		log.Error(err, "[UserPrivs] Show grants failed")
		return nil, err
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		log.Error(err, "[UserPrivs] Failed to get columns")
		return nil, err
	}

	if rows.Next() {
		var Grant Grant
		var scanArgs []interface{}

		if len(columns) == 11 { // Doris 2
			scanArgs = []interface{}{
				&Grant.UserIdentity,
				&Grant.Comment,
				&Grant.Password,
				&Grant.Roles,
				&Grant.GlobalPrivs,
				&Grant.CatalogPrivs,
				&Grant.DatabasePrivs,
				&Grant.TablePrivs,
				&Grant.ColPrivs,
				&Grant.ResourcePrivs,
				&Grant.WorkloadGroupPrivs,
			}
		} else if len(columns) == 15 { // Doris 3
			scanArgs = []interface{}{
				&Grant.UserIdentity,
				&Grant.Comment,
				&Grant.Password,
				&Grant.Roles,
				&Grant.GlobalPrivs,
				&Grant.CatalogPrivs,
				&Grant.DatabasePrivs,
				&Grant.TablePrivs,
				&Grant.ColPrivs,
				&Grant.ResourcePrivs,
				&Grant.CloudClusterPrivs,
				&Grant.CloudStagePrivs,
				&Grant.StorageVaultPrivs,
				&Grant.WorkloadGroupPrivs,
				&Grant.ComputeGroupPrivs,
			}
		} else {
			log.Error(fmt.Errorf("unexpected number of columns"), "[UserPrivs] Unexpected number of columns", "columns", len(columns))
			return nil, fmt.Errorf("unexpected number of columns: %d", len(columns))
		}

		err := rows.Scan(scanArgs...)
		if err != nil {
			log.Error(err, "[UserPrivs] Read row failed")
			return nil, err
		}

		log.Info("[UserPrivs] Scanned row", "Grant", Grant)

		entries := []struct {
			privs      sql.NullString
			entityType EntityType
		}{
			{Grant.GlobalPrivs, Table},
			{Grant.CatalogPrivs, Table},
			{Grant.DatabasePrivs, Table},
			{Grant.TablePrivs, Table},
			{Grant.ColPrivs, Table},
			{Grant.ResourcePrivs, Resource},
			{Grant.WorkloadGroupPrivs, WorkloadGroup},
		}

		for _, entry := range entries {
			if builtGrants, err := buildGrants(entry.privs, entry.entityType); err != nil {
				log.Error(err, "[UserPrivs] Build grants failed")
				return nil, err
			} else {
				grants = append(grants, builtGrants...)
			}
		}
	}
	return grants, nil
}

func (r *MySQLUserReconciler) grantPrivileges(ctx context.Context, mysqlClient *sql.DB, userIdentity string, grant mysqlv1alpha1.Grant) error {
	log := log.FromContext(ctx)
	_, err := mysqlClient.ExecContext(ctx, fmt.Sprintf("GRANT %s ON %s TO %s;", strings.Join(grant.Privileges, ","), grant.Target, userIdentity))
	if err != nil {
		return err
	}
	log.Info("[UserPrivs] Grant", "userIdentity", userIdentity, "privileges", grant.Privileges, "target", grant.Target)
	return nil
}

func (r *MySQLUserReconciler) revokePrivileges(ctx context.Context, mysqlClient *sql.DB, userIdentity string, grants []mysqlv1alpha1.Grant) error {
	log := log.FromContext(ctx)
	for _, grant := range grants {
		_, err := mysqlClient.ExecContext(ctx, fmt.Sprintf("REVOKE %s ON %s FROM %s;", strings.Join(grant.Privileges, ","), grant.Target, userIdentity))
		if err != nil {
			log.Error(err, "[UserPrivs] Revoke failed: %w", err)
			return err
		}
		log.Info("[UserPrivs] Revoke", "userIdentity", userIdentity, "privileges", grant.Privileges, "target", grant.Target)
	}
	return nil
}

func comparePrivileges(oldPrivileges, newPrivileges []string) (revokePrivileges, addPrivileges []string) {
	oldPrivilegeSet := make(map[string]struct{})
	newPrivilegeSet := make(map[string]struct{})

	for _, priv := range oldPrivileges {
		oldPrivilegeSet[priv] = struct{}{}
	}

	for _, priv := range newPrivileges {
		newPrivilegeSet[priv] = struct{}{}
	}

	for priv := range oldPrivilegeSet {
		if _, found := newPrivilegeSet[priv]; !found {
			revokePrivileges = append(revokePrivileges, priv)
		}
	}

	for priv := range newPrivilegeSet {
		if _, found := oldPrivilegeSet[priv]; !found {
			addPrivileges = append(addPrivileges, priv)
		}
	}

	return revokePrivileges, addPrivileges
}

func calculateGrantDiff(oldGrants, newGrants []mysqlv1alpha1.Grant) (grantsToRevoke, grantsToAdd []mysqlv1alpha1.Grant) {
	oldGrantMap := make(map[string]mysqlv1alpha1.Grant)
	newGrantMap := make(map[string]mysqlv1alpha1.Grant)

	for _, grant := range oldGrants {
		oldGrantMap[grant.Target] = grant
	}

	for _, grant := range newGrants {
		newGrantMap[grant.Target] = grant
	}

	for target, oldGrant := range oldGrantMap {
		if newGrant, found := newGrantMap[target]; found {
			// Compare privileges and determine partial revocation and addition
			revokePrivileges, addPrivileges := comparePrivileges(oldGrant.Privileges, newGrant.Privileges)
			if len(revokePrivileges) > 0 {
				grantsToRevoke = append(grantsToRevoke, mysqlv1alpha1.Grant{
					Target:     oldGrant.Target,
					Privileges: revokePrivileges,
				})
			}
			if len(addPrivileges) > 0 {
				grantsToAdd = append(grantsToAdd, mysqlv1alpha1.Grant{
					Target:     newGrant.Target,
					Privileges: addPrivileges,
				})
			}
		} else {
			grantsToRevoke = append(grantsToRevoke, oldGrant)
		}
	}

	for target, newGrant := range newGrantMap {
		if _, found := oldGrantMap[target]; !found {
			grantsToAdd = append(grantsToAdd, newGrant)
		}
	}

	return grantsToRevoke, grantsToAdd
}

func (r *MySQLUserReconciler) updateGrants(ctx context.Context, mysqlClient *sql.DB, userIdentity string, grants []mysqlv1alpha1.Grant) error {
	// Fetch existing grants
	existingGrants, fetchErr := fetchExistingGrants(ctx, mysqlClient, userIdentity)
	if fetchErr != nil {
		return fetchErr
	}

	// Normalize grants
	for i := range grants {
		grants[i].Privileges = normalizePerms(grants[i].Privileges)
	}

	// Calculate grants to revoke and grants to add
	grantsToRevoke, grantsToAdd := calculateGrantDiff(existingGrants, grants)

	// Revoke obsolete grants
	revokeErr := r.revokePrivileges(ctx, mysqlClient, userIdentity, grantsToRevoke)
	if revokeErr != nil {
		return revokeErr
	}

	// Grant missing grants
	for _, grant := range grantsToAdd {
		grantErr := r.grantPrivileges(ctx, mysqlClient, userIdentity, grant)
		if grantErr != nil {
			return grantErr
		}
	}

	return nil
}
