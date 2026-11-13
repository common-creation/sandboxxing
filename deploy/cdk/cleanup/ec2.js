// Guests are allocated by the daemon and outlive individual connections. On
// stack deletion drain them before CloudFormation deletes the VPC and EFS.
const ec2 = require('@aws-sdk/client-ec2');
const efs = require('@aws-sdk/client-efs');
const iam = require('@aws-sdk/client-iam');

async function terminateGuests(fileSystemId) {
  const client = new ec2.EC2Client({});
  const pages = ec2.paginateDescribeInstances(
    { client },
    {
      Filters: [
        { Name: 'tag:sandboxxing:filesystem', Values: [fileSystemId] },
        { Name: 'instance-state-name', Values: ['pending', 'running', 'stopping', 'stopped', 'shutting-down'] },
      ],
    },
  );
  const ids = [];
  for await (const page of pages) {
    for (const reservation of page.Reservations ?? []) {
      for (const instance of reservation.Instances ?? []) ids.push(instance.InstanceId);
    }
  }
  for (let i = 0; i < ids.length; i += 100) {
    const InstanceIds = ids.slice(i, i + 100);
    await client.send(new ec2.TerminateInstancesCommand({ InstanceIds }));
  }
  await Promise.all(
    Array.from({ length: Math.ceil(ids.length / 100) }, (_, i) =>
      ec2.waitUntilInstanceTerminated({ client, maxWaitTime: 600 }, { InstanceIds: ids.slice(i * 100, (i + 1) * 100) }),
    ),
  );
}

async function deleteShareAccessPoints(fileSystemId) {
  const client = new efs.EFSClient({});
  const pages = efs.paginateDescribeAccessPoints({ client }, { FileSystemId: fileSystemId });
  const ids = [];
  for await (const page of pages) {
    for (const ap of page.AccessPoints ?? []) {
      if (ap.Tags?.some((tag) => tag.Key === 'sandboxxing:share')) ids.push(ap.AccessPointId);
    }
  }
  for (const id of ids) await client.send(new efs.DeleteAccessPointCommand({ AccessPointId: id }));
}

async function removeGuestIdentities(PathPrefix) {
  const client = new iam.IAMClient({});
  const profiles = [];
  for await (const page of iam.paginateListInstanceProfiles({ client }, { PathPrefix })) {
    profiles.push(...page.InstanceProfiles);
  }
  for (const profile of profiles) {
    const InstanceProfileName = profile.InstanceProfileName;
    for (const role of profile.Roles) {
      await client.send(new iam.RemoveRoleFromInstanceProfileCommand({ InstanceProfileName, RoleName: role.RoleName }));
    }
    await client.send(new iam.DeleteInstanceProfileCommand({ InstanceProfileName }));
  }
  const roles = [];
  for await (const page of iam.paginateListRoles({ client }, { PathPrefix })) roles.push(...page.Roles);
  for (const role of roles) {
    const RoleName = role.RoleName;
    const policies = [];
    for await (const page of iam.paginateListRolePolicies({ client }, { RoleName })) policies.push(...page.PolicyNames);
    for (const PolicyName of policies) await client.send(new iam.DeleteRolePolicyCommand({ RoleName, PolicyName }));
    await client.send(new iam.DeleteRoleCommand({ RoleName }));
  }
}

exports.handler = async (event) => {
  if (event.RequestType === 'Delete') {
    await terminateGuests(process.env.FILE_SYSTEM_ID);
    await deleteShareAccessPoints(process.env.FILE_SYSTEM_ID);
    await removeGuestIdentities(process.env.ROLE_PATH);
  }
  return { PhysicalResourceId: 'sandboxxing-ec2-cleanup' };
};
