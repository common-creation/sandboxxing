// Removes what the sandboxxing daemon created outside of the stack before the
// stack is deleted: the MicroVMs of the image, the access points of the home
// directories and of the shared directories, and the execution roles of the
// containers.
const efs = require('@aws-sdk/client-efs');
const iam = require('@aws-sdk/client-iam');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function terminateMicroVMs(imageArn) {
  let mv;
  try {
    mv = require('@aws-sdk/client-lambda-microvms');
  } catch (e) {
    console.log('the Lambda MicroVMs client is not available, MicroVMs are not terminated', e);
    return;
  }
  const client = new mv.LambdaMicrovmsClient({});
  const pending = [];
  let nextToken;
  do {
    const out = await client.send(new mv.ListMicrovmsCommand({ imageIdentifier: imageArn, nextToken }));
    for (const item of out.items ?? []) {
      if (item.state === 'TERMINATED') continue;
      await client.send(new mv.TerminateMicrovmCommand({ microvmIdentifier: item.microvmId })).catch((e) => console.log(e));
      pending.push(item.microvmId);
    }
    nextToken = out.nextToken;
  } while (nextToken);
  // The connector can only be deleted once no MicroVM uses it.
  for (const id of pending) {
    for (let i = 0; i < 60; i++) {
      const out = await client.send(new mv.GetMicrovmCommand({ microvmIdentifier: id })).catch(() => undefined);
      if (!out || out.state === 'TERMINATED') break;
      await sleep(5000);
    }
  }
}

async function deleteAccessPoints(fileSystemId) {
  const client = new efs.EFSClient({});
  let NextToken;
  do {
    const out = await client.send(new efs.DescribeAccessPointsCommand({ FileSystemId: fileSystemId, NextToken }));
    for (const ap of out.AccessPoints ?? []) {
      if (!(ap.Tags ?? []).some((t) => t.Key === 'sandboxxing:instance' || t.Key === 'sandboxxing:share')) continue;
      await client.send(new efs.DeleteAccessPointCommand({ AccessPointId: ap.AccessPointId }));
    }
    NextToken = out.NextToken;
  } while (NextToken);
}

async function deleteRoles(pathPrefix) {
  const client = new iam.IAMClient({});
  let Marker;
  do {
    const out = await client.send(new iam.ListRolesCommand({ PathPrefix: pathPrefix, Marker }));
    for (const role of out.Roles ?? []) {
      const policies = await client.send(new iam.ListRolePoliciesCommand({ RoleName: role.RoleName }));
      for (const name of policies.PolicyNames ?? []) {
        await client.send(new iam.DeleteRolePolicyCommand({ RoleName: role.RoleName, PolicyName: name }));
      }
      await client.send(new iam.DeleteRoleCommand({ RoleName: role.RoleName }));
    }
    Marker = out.IsTruncated ? out.Marker : undefined;
  } while (Marker);
}

exports.handler = async (event) => {
  if (event.RequestType === 'Delete') {
    await terminateMicroVMs(process.env.IMAGE_ARN);
    await deleteAccessPoints(process.env.FILE_SYSTEM_ID);
    await deleteRoles(process.env.ROLE_PATH);
  }
  return { PhysicalResourceId: 'sandboxxing-cleanup' };
};
