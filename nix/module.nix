# NixOS module for the heka gateway.
#
# Example (flake-based host config):
#
#   inputs.heka.url = "github:idrevnii/heka";
#
#   # configuration.nix
#   imports = [ inputs.heka.nixosModules.default ];
#
#   services.heka = {
#     enable = true;
#     # Secrets never enter the nix store: the config references env vars,
#     # the env file is delivered by sops-nix / agenix / manually.
#     environmentFiles = [ config.sops.secrets."heka.env".path ];
#     settings = {
#       listen = "127.0.0.1:8787";
#       # ''${VAR} escapes nix interpolation and lands as ${VAR} in the YAML.
#       auth.tokens = [ "''${HEKA_TOKEN}" ];
#       providers.anthropic = {
#         base_url = "https://api.anthropic.com";
#         key_in.header = "x-api-key";
#         keys = [ "''${ANTHROPIC_KEY_1}" "''${ANTHROPIC_KEY_2}" ];
#       };
#     };
#   };
{ config, lib, pkgs, ... }:

let
  cfg = config.services.heka;
  yamlFormat = pkgs.formats.yaml { };
  configFile =
    if cfg.configFile != null
    then cfg.configFile
    else yamlFormat.generate "heka.yaml" cfg.settings;
in
{
  options.services.heka = {
    enable = lib.mkEnableOption "heka AI gateway";

    package = lib.mkOption {
      type = lib.types.package;
      description = "heka package to run.";
    };

    settings = lib.mkOption {
      type = yamlFormat.type;
      default = { };
      description = ''
        heka configuration rendered to YAML. Put secrets behind ''${VAR}
        references and deliver the variables via {option}`environmentFiles`.
      '';
    };

    configFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Pre-rendered config file; overrides {option}`settings`.";
    };

    environmentFiles = lib.mkOption {
      type = lib.types.listOf lib.types.path;
      default = [ ];
      description = ''
        systemd EnvironmentFile(s) with `KEY=value` lines providing the
        variables referenced from the config (gateway token, provider keys).
      '';
    };

    extraPackages = lib.mkOption {
      type = lib.types.listOf lib.types.package;
      default = [ ];
      description = ''
        Packages added to the service PATH, e.g. the CLIProxyAPI binary when
        a sidecar `command` is configured.
      '';
    };

    logJson = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Log as JSON (journald-friendly key=value otherwise).";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.settings != { } || cfg.configFile != null;
        message = "services.heka: set either settings or configFile";
      }
    ];

    systemd.services.heka = {
      description = "heka AI gateway";
      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      path = cfg.extraPackages;

      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [ (lib.getExe cfg.package) "-config" configFile ]
          ++ lib.optional cfg.logJson "-log-json"
        );
        EnvironmentFile = cfg.environmentFiles;
        Restart = "always";
        RestartSec = 2;

        DynamicUser = true;
        # Writable home for a supervised sidecar (CLIProxyAPI keeps its
        # OAuth token store under $HOME).
        StateDirectory = "heka";
        Environment = [ "HOME=/var/lib/heka" ];

        # Hardening: heka needs the network and nothing else.
        NoNewPrivileges = true;
        CapabilityBoundingSet = "";
        AmbientCapabilities = "";
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        PrivateDevices = true;
        ProtectClock = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        ProtectProc = "invisible";
        ProcSubset = "pid";
        RestrictAddressFamilies = [ "AF_INET" "AF_INET6" "AF_UNIX" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [ "@system-service" "~@privileged" ];
        UMask = "0077";
      };
    };
  };
}
