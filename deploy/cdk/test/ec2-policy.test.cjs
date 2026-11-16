const assert = require('node:assert/strict');
const { isDeepStrictEqual } = require('node:util');
const { test } = require('node:test');
const { App, Aws } = require('aws-cdk-lib');
const { Template } = require('aws-cdk-lib/assertions');
const { SubnetType } = require('aws-cdk-lib/aws-ec2');
const { SandboxxingEC2Stack } = require('../lib/ec2-stack.ts');

// Synthesizing the real stack uses the same assets as make cdk-ec2-synth.
const stack = new SandboxxingEC2Stack(new App(), 'PolicyTest');
const template = Template.fromStack(stack).toJSON();
const policyId = template.Outputs.DaemonPolicyArn.Value.Ref;
const statements = template.Resources[policyId].Properties.PolicyDocument.Statement;
const grants = [];
for (const statement of statements.filter(s => s.Effect === 'Allow')) {
  for (const resource of [statement.Resource].flat()) {
    for (const action of [statement.Action].flat()) {
      grants.push({ action, resource, condition: statement.Condition });
    }
  }
}

const shares = stack.node.findChild('Shares');
const regionalArn = service => `arn:${Aws.PARTITION}:${service}:${Aws.REGION}:${Aws.ACCOUNT_ID}:`;
const instance = regionalArn('ec2') + 'instance/*';
const volume = regionalArn('ec2') + 'volume/*';
const ownership = { StringEquals: { 'aws:ResourceTag/sandboxxing:filesystem': shares.fileSystemId } };

function checkGrant(action, resource, condition) {
  const expectedResource = stack.resolve(resource);
  const expectedCondition = condition && stack.resolve(condition);
  assert.ok(grants.some(grant => grant.action === action && isDeepStrictEqual(grant.resource, expectedResource)
    && isDeepStrictEqual(grant.condition, expectedCondition)), `missing grant for ${action} on ${resource}`);
}

test('EC2 launch, lifecycle and resize grants match their resource types and ownership tags', () => {
  for (const action of ['DescribeImages', 'DescribeInstances', 'DescribeInstanceTypes', 'DescribeVolumes',
    'DescribeVolumesModifications', 'DescribeInstanceStatus']) {
    checkGrant('ec2:' + action, '*');
  }
  const guest = stack.node.findChild('Guest');
  const vpc = stack.node.findChild('Vpc');
  checkGrant('ec2:RunInstances', `arn:${Aws.PARTITION}:ec2:${Aws.REGION}::image/*`);
  checkGrant('ec2:RunInstances', regionalArn('ec2') + 'network-interface/*');
  checkGrant('ec2:RunInstances', regionalArn('ec2') + 'security-group/' + guest.securityGroupId);
  for (const subnet of vpc.selectSubnets({ subnetType: SubnetType.PRIVATE_WITH_EGRESS }).subnetIds) {
    checkGrant('ec2:RunInstances', regionalArn('ec2') + 'subnet/' + subnet);
  }
  for (const resource of [instance, volume]) {
    checkGrant('ec2:RunInstances', resource, {
      StringEquals: { 'aws:RequestTag/sandboxxing:filesystem': shares.fileSystemId },
    });
    checkGrant('ec2:CreateTags', resource, { StringEquals: { 'ec2:CreateAction': 'RunInstances' } });
  }
  for (const action of ['StartInstances', 'StopInstances', 'RebootInstances', 'TerminateInstances',
    'ModifyInstanceAttribute']) {
    checkGrant('ec2:' + action, instance, ownership);
  }
  checkGrant('ec2:ModifyVolume', volume, ownership);
});

test('missing IAM names are authorized only for lookup and cleanup', () => {
  const iamArn = `arn:${Aws.PARTITION}:iam::${Aws.ACCOUNT_ID}:`;
  const guestPath = `/sandboxxing/ec2/${shares.fileSystemId}/sbx-ec2-*`;
  const role = iamArn + 'role' + guestPath;
  const profile = iamArn + 'instance-profile' + guestPath;
  const boundary = stack.node.findChild('GuestBoundary');
  checkGrant('iam:CreateRole', role, { StringEquals: { 'iam:PermissionsBoundary': boundary.managedPolicyArn } });
  checkGrant('iam:PassRole', role, { StringEquals: { 'iam:PassedToService': 'ec2.amazonaws.com' } });
  for (const action of ['TagRole', 'PutRolePolicy', 'GetRole', 'DeleteRolePolicy', 'DeleteRole']) {
    checkGrant('iam:' + action, role);
  }
  for (const action of ['CreateInstanceProfile', 'TagInstanceProfile', 'GetInstanceProfile', 'AddRoleToInstanceProfile',
    'RemoveRoleFromInstanceProfile', 'DeleteInstanceProfile']) {
    checkGrant('iam:' + action, profile);
  }
  const name = 'sbx-ec2-' + '?'.repeat(32);
  for (const [type, actions] of [
    ['role', ['iam:GetRole', 'iam:DeleteRolePolicy', 'iam:DeleteRole']],
    ['instance-profile', ['iam:GetInstanceProfile', 'iam:RemoveRoleFromInstanceProfile', 'iam:DeleteInstanceProfile']],
  ]) {
    const resource = stack.resolve(`${iamArn}${type}/${name}`);
    const actual = grants.filter(grant => isDeepStrictEqual(grant.resource, resource));
    assert.deepEqual(actual.map(grant => grant.action).sort(), actions.sort(), type);
    for (const grant of actual) {
      assert.deepEqual(grant.condition, { Null: { 'aws:ResourceTag/sandboxxing:filesystem': 'true' } }, grant.action);
    }
  }
});

test('daemon can create tagged access points and open SSM sessions on its guests', () => {
  checkGrant('elasticfilesystem:DescribeAccessPoints', '*');
  checkGrant('elasticfilesystem:CreateAccessPoint', shares.fileSystemArn);
  checkGrant('elasticfilesystem:TagResource', shares.fileSystemArn, {
    StringEquals: { 'elasticfilesystem:CreateAction': 'CreateAccessPoint' },
  });
  checkGrant('ssm:StartSession', instance, {
    StringEquals: { 'ssm:resourceTag/sandboxxing:filesystem': shares.fileSystemId },
  });
  checkGrant('ssm:StartSession', `arn:${Aws.PARTITION}:ssm:${Aws.REGION}::document/AWS-StartSSHSession`);
  for (const action of ['ssm:TerminateSession', 'ssmmessages:OpenDataChannel']) {
    checkGrant(action, regionalArn('ssm') + 'session/*', {
      StringEquals: { 'ssm:resourceTag/aws:ssmmessages:session-id': '${aws:userid}' },
    });
  }
});
