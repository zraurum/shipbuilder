package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"text/template"
)

const (
	PRE_RECEIVE = `#!/bin/bash
while read oldrev newrev refname
do
  ` + EXE + ` pre-receive ` + "`pwd`" + ` $oldrev $newrev $refname || exit 1
done
`
	POST_RECEIVE = `#!/bin/bash
while read oldrev newrev refname
do
  ` + EXE + ` post-receive ` + "`pwd`" + ` $oldrev $newrev $refname || exit 1
done
`
)

var POSTDEPLOY = `#!/usr/bin/python -u
# -*- coding: utf-8 -*-

import os, stat, subprocess, sys, time

container = None
log = lambda message: sys.stdout.write('[{0}] {1}\n'.format(container, message))

def getIp(name):
    with open('/var/lib/lxc/' + name + '/rootfs/app/ip') as f:
        return f.read().split('/')[0]

def modifyIpTables(action, chain, ip, port):
    """
    @param action str 'append' or 'delete'.
    @param chain str 'PREROUTING' or 'OUTPUT'.
    """
    assert action in ('append', 'delete'), 'Invalid action: "{0}", must be "append" or "delete"'
    assert chain in ('PREROUTING', 'OUTPUT'), 'Invalid chain: "{0}", must be "PREROUTING" or "OUTPUT"'.format(chain)
    assert ip is not None and ip != '', 'Invalid ip: "{0}", ip cannot be None or empty'.format(ip)
    assert port is not None and port != '', 'Invalid port: "{0}", port cannot be None or empty'.format(port)

    # Sometimes iptables is being run too many times at once on the same box, and will give an error like:
    #     iptables: Resource temporarily unavailable.
    #     exit status 4
    # We try to detect any such occurrence, and up to N times we'll wait for a moment and retry.
    attempts = 0
    while True:
        child = subprocess.Popen(
            [
                '/sbin/iptables',
                '--table', 'nat',
                '--{0}'.format(action), chain,
                '--proto', 'tcp',
                '--dport', port,
                '--jump', 'DNAT',
                '--to-destination', '{0}:{1}'.format(ip, port),
            ] + (['--out-interface', 'lo'] if chain == 'OUTPUT' else []),
            stderr=sys.stderr,
            stdout=sys.stdout
        )
        child.communicate()
        exitCode = child.returncode
        if exitCode == 0:
            return
        elif exitCode == 4 and attempts < 5:
            log('iptables: Resource temporarily unavailable (exit status 4), retrying.. ({0} previous attempts)'.format(attempts))
            attempts += 1
            time.sleep(1)
            continue
        else:
            raise subprocess.CalledProcessError('iptables exited with status code {0}'.format(exitCode))

def ipsForRulesMatchingPort(chain, port):
    # NB: 'exit 0' added to avoid exit status code 1 when there were no results.
    rawOutput = subprocess.check_output(
        [
            '/sbin/iptables --table nat --list {0} --numeric | grep -E -o "[0-9.]+:{1}" | grep -E -o "^[^:]+"; exit 0' \
                .format(chain, port),
        ],
        shell=True,
        stderr=sys.stderr
    ).strip()
    return rawOutput.split('\n') if len(rawOutput) > 0 else []

def configureIpTablesForwarding(ip, port):
    log('configuring iptables to forward port {0} to {1}'.format(port, ip))
    # Clear out any conflicting pre-existing rules on the same port.
    for chain in ('PREROUTING', 'OUTPUT'):
        conflictingRules = ipsForRulesMatchingPort(chain, port)
        for someOtherIp in conflictingRules:
            modifyIpTables('delete', chain, someOtherIp, port)

    # Add a rule to route <eth0-iface>:<port> TCP packets to the container.
    modifyIpTables('append', 'PREROUTING', ip, port)

    # Add another rule so that the port will be reachable from <eth0-iface>:port from localhost.
    modifyIpTables('append', 'OUTPUT', ip, port)

def main(argv):
    global container
    #print 'main argv={0}'.format(argv)
    container = argv[1]
    process = argv[1].split('` + DYNO_DELIMITER + `')[-3] # Process is always 3 from the end.

    app = container.rsplit('` + DYNO_DELIMITER + `', 3)[0] # Get rid of port + version.
    port = container.split('` + DYNO_DELIMITER + `')[-1] # Port is always at the end.

    # For safety, even though it's unlikley, try to kill/shutdown any existing container with the same name.
    subprocess.call(['/usr/bin/lxc-stop -k -n {0} 1>&2 2>/dev/null'.format(container)], shell=True)
    subprocess.call(['/usr/bin/lxc-destroy -n {0} 1>&2 2>/dev/null'.format(container)], shell=True)

    # Start the specified container.
    log('cloning container: {0}'.format(container))
    subprocess.check_call(
        ['/usr/bin/lxc-clone', '-s', '-B', 'btrfs', '-o', app, '-n', container],
        stdout=sys.stdout,
        stderr=sys.stderr
    )

    # This line, if present, will prevent the container from booting.
    #log('scrubbing any "lxc.cap.drop = mac_{0}" lines from container config'.format(container))
    subprocess.check_call(
        ['sed', '-i', '/lxc.cap.drop = mac_{0}/d'.format(container), '/var/lib/lxc/{0}/config'.format(container)],
        stdout=sys.stdout,
        stderr=sys.stderr
    )

    log('creating run script for app "{0}" with process type={1}'.format(app, process))
    # NB: The curly braces are kinda crazy here, to get a single '{' or '}' with python.format(), use double curly
    # braces.
    host = '''` + sshHost + `'''
    runScript = '''#!/bin/bash
ip addr show eth0 | grep 'inet.*eth0' | awk '{{print $2}}' > /app/ip
rm -rf /tmp/log
cd /app/src
echo '{port}' > ../env/PORT
while read line || [ -n "$line" ]; do
    process="${{line%%:*}}"
    command="${{line#*: }}"
    if [ "$process" == "{process}" ]; then
        envdir ../env /bin/bash -c "${{command}} 2>&1 | /app/` + BINARY + ` logger -h{host} -a{app} -p{process}"
    fi
done < Procfile'''.format(port=port, host=host.split('@')[-1], process=process, app=app)
    runScriptFileName = '/var/lib/lxc/{0}/rootfs/app/run'.format(container)
    with open(runScriptFileName, 'w') as fh:
        fh.write(runScript)
    # Chmod to be executable.
    st = os.stat(runScriptFileName)
    os.chmod(runScriptFileName, st.st_mode | stat.S_IEXEC)

    log('starting container')
    subprocess.check_call(
        ['/usr/bin/lxc-start', '--daemon', '-n', container],
        stdout=sys.stdout,
        stderr=sys.stderr
    )

    log('waiting for container to boot and report ip-address')
    # Allow container to bootup.
    ip = None
    for _ in xrange(45):
        time.sleep(1)
        try:
            ip = getIp(container)
        except:
            continue

    if ip:
        log('found ip: {0}'.format(ip))
        configureIpTablesForwarding(ip, port)

        if process == 'web':
            log('waiting for web-server to finish starting up')
            try:
                subprocess.check_call([
                    '/usr/bin/curl',
                    '-sL',
                    '-w', '"%{http_code} %{url_effective}\\n"',
                    '{0}:{1}/'.format(ip, port),
                    '-o', '/dev/null',
                ], stderr=sys.stderr, stdout=sys.stdout)
            except subprocess.CalledProcessError, e:
                sys.stderr.write('- curl http check failed: {0}\n'.format(e))

    else:
        log('- error retrieving ip')
        sys.exit(1)

main(sys.argv)`

var SHUTDOWN_CONTAINER = `#!/usr/bin/python -u
# -*- coding: utf-8 -*-

import subprocess, sys, time

container = None
log = lambda message: sys.stdout.write('[{0}] {1}\n'.format(container, message))

def modifyIpTables(action, chain, ip, port):
    """
    @param action str 'append' or 'delete'.
    @param chain str 'PREROUTING' or 'OUTPUT'.
    """
    assert action in ('append', 'delete'), 'Invalid action: "{0}", must be "append" or "delete"'
    assert chain in ('PREROUTING', 'OUTPUT'), 'Invalid chain: "{0}", must be "PREROUTING" or "OUTPUT"'.format(chain)
    assert ip is not None and ip != '', 'Invalid ip: "{0}", ip cannot be None or empty'.format(ip)
    assert port is not None and port != '', 'Invalid port: "{0}", port cannot be None or empty'.format(port)

    # Sometimes iptables is being run too many times at once on the same box, and will give an error like:
    #     iptables: Resource temporarily unavailable.
    #     exit status 4
    # We try to detect any such occurrence, and up to N times we'll wait for a moment and retry.
    attempts = 0
    while True:
        child = subprocess.Popen(
            [
                '/sbin/iptables',
                '--table', 'nat',
                '--{0}'.format(action), chain,
                '--proto', 'tcp',
                '--dport', port,
                '--jump', 'DNAT',
                '--to-destination', '{0}:{1}'.format(ip, port),
            ] + (['--out-interface', 'lo'] if chain == 'OUTPUT' else []),
            stderr=sys.stderr,
            stdout=sys.stdout
        )
        child.communicate()
        exitCode = child.returncode
        if exitCode == 0:
            return
        elif exitCode == 4 and attempts < 5:
            log('iptables: Resource temporarily unavailable (exit status 4), retrying.. ({0} previous attempts)'.format(attempts))
            attempts += 1
            time.sleep(1)
            continue
        else:
            raise subprocess.CalledProcessError('iptables exited with status code {0}'.format(exitCode))

def ipsForRulesMatchingPort(chain, port):
    # NB: 'exit 0' added to avoid exit status code 1 when there were no results.
    rawOutput = subprocess.check_output(
        [
            '/sbin/iptables --table nat --list {0} --numeric | grep -E -o "[0-9.]+:{1}" | grep -E -o "^[^:]+"; exit 0' \
                .format(chain, port),
        ],
        shell=True,
        stderr=sys.stderr
    ).strip()
    return rawOutput.split('\n') if len(rawOutput) > 0 else []

def main(argv):
    global container
    container = argv[1]
    port = container.split('` + DYNO_DELIMITER + `').pop()

    # Stop all existing containers.
    log('stopping container')
    subprocess.check_call(['/usr/bin/lxc-stop', '-k', '-n', container], stdout=sys.stdout, stderr=sys.stderr)
    subprocess.check_call(['/usr/bin/lxc-destroy', '-n', container], stdout=sys.stdout, stderr=sys.stderr)

    for chain in ('PREROUTING', 'OUTPUT'):
        rules = ipsForRulesMatchingPort(chain, port)
        for ip in rules:
            log('removing iptables {0} chain rule: port={1} ip={2}'.format(chain, port, ip))
            modifyIpTables('delete', chain, ip, port)

main(sys.argv)`

var (
	UPSTART        = template.New("UPSTART")
	HAPROXY_CONFIG = template.New("HAPROXY_CONFIG")
	BUILD_PACKS    = map[string]*template.Template{}
)

func init() {
	// Only validate templates if not running in server-mode.
	if len(os.Args) > 1 && os.Args[1] != "server" {
		return
	}

	template.Must(UPSTART.Parse(`
console none

start on (local-filesystems and net-device-up IFACE!=lo)
stop on [!12345]
#exec su ` + DEFAULT_NODE_USERNAME + ` -c "/app/run"
#exec /app/run
exec start-stop-daemon --start -u ubuntu --exec /app/run`))

	template.Must(HAPROXY_CONFIG.Parse(`
global
    maxconn 4096
    # NB: Base HAProxy logging configuration is as per: http://kvz.io/blog/2010/08/11/haproxy-logging/
    log 127.0.0.1 local1 info

defaults
    log global
    mode http
    option tcplog
    retries 4
    option redispatch
    maxconn 32000
    contimeout 5000
    clitimeout 30000
    srvtimeout 30000
    timeout client 30000
    #option http-server-close

frontend frontend
    bind 0.0.0.0:80
    # Require SSL
    redirect scheme https if !{ ssl_fc }
    bind 0.0.0.0:443 ssl crt /etc/haproxy/certs.d
    option httplog
    option http-pretend-keepalive
    option forwardfor
    option http-server-close
{{range $app := .Applications}}
  {{range .Domains}}
    use_backend {{$app.Name}}{{if $app.Maintenance}}-maintenance{{end}} if { hdr_dom(host) -i {{.}} }
  {{end}}
{{end}}

{{range .Applications}}
backend {{.Name}}
    balance roundrobin
    reqadd X-Forwarded-Proto:\ https if { ssl_fc }
    option forwardfor
    option abortonclose
    option httpchk GET /
  {{range .Servers}}
    #server {{.Host}}-{{.Port}} {{.Host}}:{{.Port}} check port {{.Port}} observe layer7 minconn 20 maxconn 40 check inter 10s rise 1 fall 3 weight 1
    server {{.Host}}-{{.Port}} {{.Host}}:{{.Port}} check port {{.Port}} observe layer7
  {{end}}{{if .HaProxyStatsEnabled}}
    stats enable
    stats uri /haproxy
    stats auth {{.HaProxyCredentials}}
  {{end}}

backend {{.Name}}-maintenance
    acl static_file path_end .gif || path_end .jpg || path_end .jpeg || path_end .png || path_end .css
    reqirep ^GET\ (.*)                    GET\ {{.MaintenancePageBasePath}}\1     if static_file
    reqirep ^([^\ ]*)\ [^\ ]*\ (.*)       \1\ {{.MaintenancePageFullPath}}\ \2    if !static_file
    reqirep ^Host:\ .*                    Host:\ {{.MaintenancePageDomain}}
    reqadd Cache-Control:\ no-cache,\ no-store,\ must-revalidate
    reqadd Pragma:\ no-cache
    reqadd Expires:\ 0
    rspirep ^HTTP/([^0-9\.]+)\ 200\ OK    HTTP/\1\ 503\ 
    rspadd Retry-After:\ 60
    server s3 {{.MaintenancePageDomain}}:80
{{end}}`))

	// Discover all available build-packs.
	listing, err := ioutil.ReadDir(DIRECTORY + "/build-packs")
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
	for _, buildPack := range listing {
		if buildPack.IsDir() {
			fmt.Printf("Discovered build-pack: %v\n", buildPack.Name())
			contents, err := ioutil.ReadFile(DIRECTORY + "/build-packs/" + buildPack.Name() + "/pre-hook")
			if err != nil {
				fmt.Fprintf(os.Stderr, "fatal: build-pack '%v' missing pre-hook file: %v\n", buildPack.Name(), err)
				os.Exit(1)
			}
			// Map to template.
			BUILD_PACKS[buildPack.Name()] = template.Must(template.New("BUILD_" + strings.ToUpper(buildPack.Name())).Parse(string(contents)))
		}
	}

	if len(BUILD_PACKS) == 0 {
		fmt.Fprintf(os.Stderr, "fatal: no build-packs found\n")
		os.Exit(1)
	}
}
