import * as fs from 'fs';
import * as path from 'path';
import { Aws, CfnOutput, CustomResource, Duration, RemovalPolicy, Stack, StackProps } from 'aws-cdk-lib';
import * as ec2 from 'aws-cdk-lib/aws-ec2';
import * as efs from 'aws-cdk-lib/aws-efs';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import * as logs from 'aws-cdk-lib/aws-logs';
import * as s3assets from 'aws-cdk-lib/aws-s3-assets';
import * as ssm from 'aws-cdk-lib/aws-ssm';
import * as cr from 'aws-cdk-lib/custom-resources';
import { Construct } from 'constructs';

const ssmGuestActions = [
  'ssm:UpdateInstanceInformation',
  'ssmmessages:CreateControlChannel',
  'ssmmessages:CreateDataChannel',
  'ssmmessages:OpenControlChannel',
  'ssmmessages:OpenDataChannel',
];
const mountActions = [
  'elasticfilesystem:ClientMount',
  'elasticfilesystem:ClientWrite',
  'elasticfilesystem:ClientRootAccess',
];

export class SandboxxingEC2Stack extends Stack {
  constructor(scope: Construct, id: string, props?: StackProps) {
    super(scope, id, props);
    // SSM, S3, and Ubuntu's package mirrors are reached outbound via NAT.
    const vpc = new ec2.Vpc(this, 'Vpc', { maxAzs: 2, natGateways: 1 });
    const subnets = vpc.selectSubnets({ subnetType: ec2.SubnetType.PRIVATE_WITH_EGRESS });
    const guest = new ec2.SecurityGroup(this, 'Guest', { vpc, description: 'Sandbox instances: outbound only' });
    const mount = new ec2.SecurityGroup(this, 'Mount', { vpc, allowAllOutbound: false });
    mount.addIngressRule(guest, ec2.Port.tcp(2049), 'Sandbox EFS mounts');
    const shares = new efs.FileSystem(this, 'Shares', {
      vpc,
      vpcSubnets: subnets,
      securityGroup: mount,
      encrypted: true,
      allowAnonymousAccess: true,
      removalPolicy: RemovalPolicy.DESTROY,
      // A resource policy without anonymous allows requires authenticated mounts.
      fileSystemPolicy: new iam.PolicyDocument({
        statements: [
          new iam.PolicyStatement({
            effect: iam.Effect.DENY,
            principals: [new iam.AnyPrincipal()],
            actions: ['elasticfilesystem:*'],
            resources: ['*'],
            conditions: { Bool: { 'aws:SecureTransport': 'false' } },
          }),
          new iam.PolicyStatement({
            effect: iam.Effect.DENY,
            principals: [new iam.AnyPrincipal()],
            actions: mountActions,
            resources: ['*'],
            conditions: { Null: { 'elasticfilesystem:AccessPointArn': 'true' } },
          }),
        ],
      }),
    });
    const data = path.join(__dirname, '../../ec2-assets');
    const agent = new s3assets.Asset(this, 'Agent', { path: path.join(data, 'sandboxxing-agent') });
    const helper = new s3assets.Asset(this, 'MountHelper', { path: path.join(data, 'amazon-efs-utils.deb') });
    const boundary = new iam.ManagedPolicy(this, 'GuestBoundary', {
      statements: [
        new iam.PolicyStatement({ actions: ssmGuestActions, resources: ['*'] }),
        new iam.PolicyStatement({
          actions: ['s3:GetObject'],
          resources: [agent.bucket.arnForObjects(agent.s3ObjectKey), helper.bucket.arnForObjects(helper.s3ObjectKey)],
        }),
        new iam.PolicyStatement({
          actions: mountActions,
          resources: [shares.fileSystemArn],
          conditions: {
            Bool: { 'elasticfilesystem:AccessedViaMountTarget': 'true' },
            Null: { 'elasticfilesystem:AccessPointArn': 'false' },
          },
        }),
      ],
    });
    // The filesystem ID distinguishes stacks across regions in global IAM.
    const rolePath = `/sandboxxing/ec2/${shares.fileSystemId}/`;
    const iamArn = `arn:${Aws.PARTITION}:iam::${Aws.ACCOUNT_ID}:`;
    const role = iamArn + 'role' + rolePath + 'sbx-ec2-*';
    const profile = iamArn + 'instance-profile' + rolePath + 'sbx-ec2-*';
    // Get and cleanup calls take only a name. Until an object exists IAM
    // can authorize the request against that name without the stack path.
    // The daemon names these objects sbx-ec2- followed by a 32-character token.
    const guestName = 'sbx-ec2-' + '?'.repeat(32);
    const absentRole = iamArn + 'role/' + guestName;
    const absentProfile = iamArn + 'instance-profile/' + guestName;
    const untagged = { Null: { 'aws:ResourceTag/sandboxxing:filesystem': 'true' } };
    const ec2Arn = `arn:${Aws.PARTITION}:ec2:${Aws.REGION}:${Aws.ACCOUNT_ID}:`;
    const sessionArn = `arn:${Aws.PARTITION}:ssm:${Aws.REGION}:${Aws.ACCOUNT_ID}:session/*`;
    const ap = `arn:${Aws.PARTITION}:elasticfilesystem:${Aws.REGION}:${Aws.ACCOUNT_ID}:access-point/*`;
    const belongs = { StringEquals: { 'aws:ResourceTag/sandboxxing:filesystem': shares.fileSystemId } };
    const stmt = (actions: string[], resources: string[], conditions?: Record<string, unknown>) =>
      new iam.PolicyStatement({ actions, resources, conditions });
    const policy = new iam.ManagedPolicy(this, 'DaemonPolicy', {
      statements: [
        stmt(
          [
            'ec2:DescribeImages',
            'ec2:DescribeInstances',
            'ec2:DescribeInstanceTypes',
            'ec2:DescribeVolumes',
            'ec2:DescribeVolumesModifications',
            'ec2:DescribeInstanceStatus',
          ],
          ['*'],
        ),
        stmt(
          ['ec2:RunInstances'],
          [
            `arn:${Aws.PARTITION}:ec2:${Aws.REGION}::image/*`,
            ec2Arn + 'network-interface/*',
            ec2Arn + 'security-group/' + guest.securityGroupId,
            ...subnets.subnetIds.map((s) => ec2Arn + 'subnet/' + s),
          ],
        ),
        stmt(['ec2:RunInstances'], [ec2Arn + 'instance/*', ec2Arn + 'volume/*'], {
          StringEquals: { 'aws:RequestTag/sandboxxing:filesystem': shares.fileSystemId },
        }),
        stmt(['ec2:CreateTags'], [ec2Arn + 'instance/*', ec2Arn + 'volume/*'], {
          StringEquals: { 'ec2:CreateAction': 'RunInstances' },
        }),
        stmt(
          [
            'ec2:StartInstances',
            'ec2:StopInstances',
            'ec2:RebootInstances',
            'ec2:TerminateInstances',
            'ec2:ModifyInstanceAttribute',
          ],
          [ec2Arn + 'instance/*'],
          belongs,
        ),
        stmt(['ec2:ModifyVolume'], [ec2Arn + 'volume/*'], belongs),
        stmt(['ssm:StartSession'], [ec2Arn + 'instance/*'], {
          StringEquals: { 'ssm:resourceTag/sandboxxing:filesystem': shares.fileSystemId },
        }),
        stmt(['ssm:StartSession'], [`arn:${Aws.PARTITION}:ssm:${Aws.REGION}::document/AWS-StartSSHSession`]),
        stmt(['ssm:TerminateSession', 'ssmmessages:OpenDataChannel'], [sessionArn], {
          StringEquals: { 'ssm:resourceTag/aws:ssmmessages:session-id': '${aws:userid}' },
        }),
        stmt(['elasticfilesystem:DescribeAccessPoints'], ['*']),
        stmt(['elasticfilesystem:CreateAccessPoint'], [shares.fileSystemArn]),
        stmt(['elasticfilesystem:TagResource'], [shares.fileSystemArn, ap], {
          StringEquals: { 'elasticfilesystem:CreateAction': 'CreateAccessPoint' },
        }),
        stmt(['iam:CreateRole'], [role], { StringEquals: { 'iam:PermissionsBoundary': boundary.managedPolicyArn } }),
        stmt(['iam:GetRole', 'iam:TagRole', 'iam:PutRolePolicy', 'iam:DeleteRolePolicy', 'iam:DeleteRole'], [role]),
        stmt(['iam:GetRole', 'iam:DeleteRolePolicy', 'iam:DeleteRole'], [absentRole], untagged),
        stmt(['iam:PassRole'], [role], { StringEquals: { 'iam:PassedToService': 'ec2.amazonaws.com' } }),
        stmt(
          [
            'iam:CreateInstanceProfile',
            'iam:TagInstanceProfile',
            'iam:GetInstanceProfile',
            'iam:AddRoleToInstanceProfile',
            'iam:RemoveRoleFromInstanceProfile',
            'iam:DeleteInstanceProfile',
          ],
          [profile],
        ),
        stmt(
          ['iam:GetInstanceProfile', 'iam:RemoveRoleFromInstanceProfile', 'iam:DeleteInstanceProfile'],
          [absentProfile],
          untagged,
        ),
      ],
    });
    const logGroup = new logs.LogGroup(this, 'CleanupLogs', {
      retention: logs.RetentionDays.ONE_WEEK,
      removalPolicy: RemovalPolicy.DESTROY,
    });
    const cleanup = new lambda.Function(this, 'CleanupFunction', {
      runtime: lambda.Runtime.NODEJS_22_X,
      handler: 'index.handler',
      timeout: Duration.minutes(15),
      logGroup,
      code: lambda.Code.fromInline(fs.readFileSync(path.join(__dirname, '../cleanup/ec2.js'), 'utf8')),
      environment: { FILE_SYSTEM_ID: shares.fileSystemId, ROLE_PATH: rolePath },
    });
    for (const permission of [
      stmt(
        [
          'ec2:DescribeInstances',
          'iam:ListRoles',
          'iam:ListInstanceProfiles',
          'elasticfilesystem:DescribeAccessPoints',
        ],
        ['*'],
      ),
      stmt(['ec2:TerminateInstances'], [ec2Arn + 'instance/*'], belongs),
      stmt(['elasticfilesystem:DeleteAccessPoint'], [ap], belongs),
      stmt(['iam:ListRolePolicies', 'iam:DeleteRolePolicy', 'iam:DeleteRole'], [role]),
      stmt(['iam:RemoveRoleFromInstanceProfile', 'iam:DeleteInstanceProfile'], [profile]),
    ])
      cleanup.addToRolePolicy(permission);
    const provider = new cr.Provider(this, 'CleanupProvider', { onEventHandler: cleanup, logGroup });
    const onDelete = new CustomResource(this, 'Cleanup', { serviceToken: provider.serviceToken });
    onDelete.node.addDependency(shares, guest, boundary);
    const image = ssm.StringParameter.valueForStringParameter(
      this,
      '/aws/service/canonical/ubuntu/server/24.04/stable/current/amd64/hvm/ebs-gp3/ami-id',
    );
    new CfnOutput(this, 'DaemonPolicyArn', { value: policy.managedPolicyArn });
    new CfnOutput(this, 'DaemonConfig', {
      value: this.toJsonString({
        ec2: {
          region: Aws.REGION,
          images: { 'ubuntu-24.04': image },
          default_image: 'ubuntu-24.04',
          subnet_id: subnets.subnetIds[0],
          security_group_ids: [guest.securityGroupId],
          efs_file_system_id: shares.fileSystemId,
          role_path: rolePath,
          permissions_boundary_arn: boundary.managedPolicyArn,
          agent_s3_uri: agent.s3ObjectUrl,
          efs_utils_s3_uri: helper.s3ObjectUrl,
        },
      }),
    });
  }
}
