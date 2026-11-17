const assert = require('node:assert/strict');
const { isDeepStrictEqual } = require('node:util');
const { test } = require('node:test');
const { App, Aws, Fn } = require('aws-cdk-lib');
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
  const guestPath = `/sandboxxing/ec2/${Fn.join('', Fn.split('-', shares.fileSystemId))}/sbx-ec2-*`;
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

test('daemon can create tagged access points', () => {
  checkGrant('elasticfilesystem:DescribeAccessPoints', '*');
  checkGrant('elasticfilesystem:CreateAccessPoint', shares.fileSystemArn);
  checkGrant('elasticfilesystem:TagResource', shares.fileSystemArn, {
    StringEquals: { 'elasticfilesystem:CreateAction': 'CreateAccessPoint' },
  });
});

const sessionManager = /^(ssm|ssmmessages|ec2messages):/;

test('neither the daemon nor a guest may use Systems Manager', () => {
  assert.deepEqual(grants.filter(grant => sessionManager.test(grant.action)), []);
  const boundary = stack.resolve(stack.node.findChild('GuestBoundary').managedPolicyArn).Ref;
  const allowed = template.Resources[boundary].Properties.PolicyDocument.Statement.flatMap(s => [s.Action].flat());
  assert.ok(allowed.length > 0);
  assert.deepEqual(allowed.filter(action => sessionManager.test(action) || action.includes('*')), []);
});

const groupId = (s, name) => s.resolve(s.node.findChild(name).securityGroupId);
const ingressOf = (t, group) => Object.values(t.Resources).filter(r => r.Type === 'AWS::EC2::SecurityGroupIngress'
  && isDeepStrictEqual(r.Properties.GroupId, group)).map(r => r.Properties);

test('only the daemon reaches the guest agents', () => {
  const guest = groupId(stack, 'Guest');
  const daemon = groupId(stack, 'Daemon');
  const rules = ingressOf(template, guest);
  assert.equal(rules.length, 1);
  assert.deepEqual([rules[0].IpProtocol, rules[0].FromPort, rules[0].ToPort], ['tcp', 8080, 8080]);
  assert.deepEqual(rules[0].SourceSecurityGroupId, daemon);
  assert.equal(template.Resources[guest['Fn::GetAtt'][0]].Properties.SecurityGroupIngress, undefined);
  assert.deepEqual(template.Outputs.DaemonSecurityGroupId.Value, daemon);

  const routed = new SandboxxingEC2Stack(new App({ context: { daemonCidr: '10.9.0.0/16' } }), 'RoutedTest');
  const inline = Template.fromStack(routed).toJSON().Resources[groupId(routed, 'Guest')['Fn::GetAtt'][0]];
  assert.deepEqual(inline.Properties.SecurityGroupIngress.map(r => [r.CidrIp, r.IpProtocol, r.FromPort, r.ToPort]),
    [['10.9.0.0/16', 'tcp', 8080, 8080]]);
});

const ec2Config = t => JSON.parse(t.Outputs.DaemonConfig.Value['Fn::Join'][1]
  .map(part => (typeof part === 'string' ? part : 'token')).join('')).ec2;

test('guest assets keep fixed keys across deployments', () => {
  const bucket = stack.resolve(stack.node.findChild('GuestAssets').bucketName);
  const parts = template.Outputs.DaemonConfig.Value['Fn::Join'][1];
  const text = parts.filter(part => typeof part === 'string').join('');
  assert.ok(text.includes('"agent_s3_uri":"s3://') && text.includes('/guest/sandboxxing-agent"'), text);
  assert.ok(text.includes('"efs_utils_s3_uri":"s3://') && text.includes('/guest/amazon-efs-utils.deb"'), text);
  assert.ok(!/[0-9a-f]{64}/.test(text), 'DaemonConfig must not name content-addressed asset keys');
  assert.equal(parts.filter(part => isDeepStrictEqual(part, bucket)).length, 2);

  const boundary = stack.resolve(stack.node.findChild('GuestBoundary').managedPolicyArn).Ref;
  const get = template.Resources[boundary].Properties.PolicyDocument.Statement.find(s => s.Action === 's3:GetObject');
  const arn = stack.resolve(stack.node.findChild('GuestAssets').bucketArn);
  assert.deepEqual(get.Resource, ['sandboxxing-agent', 'amazon-efs-utils.deb'].map(file =>
    ({ 'Fn::Join': ['', [arn, '/guest/' + file]] })));

  const deployments = Object.values(template.Resources).filter(r => r.Type === 'Custom::CDKBucketDeployment');
  assert.equal(deployments.length, 1);
  assert.equal(deployments[0].Properties.DestinationBucketKeyPrefix, 'guest/');
  assert.deepEqual(deployments[0].Properties.DestinationBucketName, bucket);
});

test('the stack VPC keeps sandboxes on private addresses behind NAT', () => {
  const types = Object.values(template.Resources).map(r => r.Type);
  assert.ok(types.includes('AWS::EC2::VPC') && types.includes('AWS::EC2::NatGateway'));
  assert.equal(ec2Config(template).associate_public_ip_address, undefined);
});

test('the default VPC hosts the sandboxes in its public subnets', () => {
  // Without a cached lookup CDK substitutes a dummy VPC with public subnets.
  const env = { account: '123456789012', region: 'us-east-1' };
  const lookedUp = new SandboxxingEC2Stack(new App({ context: { useDefaultVpc: 'true' } }), 'DefaultVpcTest', { env });
  const t = Template.fromStack(lookedUp).toJSON();
  const resources = Object.values(t.Resources);
  assert.deepEqual(resources.filter(r => /^AWS::EC2::(VPC|Subnet|NatGateway|InternetGateway|EIP)$/.test(r.Type)), []);
  const vpc = lookedUp.node.findChild('Vpc');
  const subnetIds = vpc.selectSubnets({ subnetType: SubnetType.PUBLIC }).subnetIds;
  assert.ok(subnetIds.length > 0);
  for (const name of ['Guest', 'Daemon', 'Mount']) {
    assert.equal(t.Resources[groupId(lookedUp, name)['Fn::GetAtt'][0]].Properties.VpcId, vpc.vpcId, name);
  }
  assert.deepEqual(resources.filter(r => r.Type === 'AWS::EFS::MountTarget').map(r => r.Properties.SubnetId), subnetIds);
  const launch = t.Resources[t.Outputs.DaemonPolicyArn.Value.Ref];
  const subnetGrants = launch.Properties.PolicyDocument.Statement.filter(s => s.Action === 'ec2:RunInstances')
    .flatMap(s => [s.Resource].flat()).map(r => JSON.stringify(r)).filter(r => r.includes('subnet/'));
  assert.equal(subnetGrants.length, subnetIds.length);
  subnetIds.forEach((id, i) => assert.ok(subnetGrants[i].includes('subnet/' + id), subnetGrants[i]));
  const config = ec2Config(t);
  assert.equal(config.subnet_id, subnetIds[0]);
  assert.equal(config.associate_public_ip_address, true);
  assert.equal(t.Outputs.VpcId.Value, vpc.vpcId);
  assert.throws(() => new SandboxxingEC2Stack(new App({ context: { useDefaultVpc: 'yes' } }), 'InvalidTest', { env }));
});
