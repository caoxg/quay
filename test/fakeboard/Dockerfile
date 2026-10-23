# A fake Arduino UNO Q for Quay's end-to-end tests: Debian + sshd + sudo (password required)
# + stand-ins for arduino-app-cli, systemctl, hostnamectl and apt-get. Not a real board — it only mimics what Quay touches.
FROM python:3.12-slim

RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends openssh-server sudo procps iproute2 >/dev/null \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /run/sshd \
    && useradd -m -s /bin/bash arduino \
    && echo 'arduino:quay-test' | chpasswd \
    && mkdir -p /home/arduino/ArduinoApps /var/lib/arduino-app-cli /etc/systemd/system \
    && chown arduino /home/arduino/ArduinoApps /var/lib/arduino-app-cli \
    && sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication yes/' /etc/ssh/sshd_config

COPY stubs/arduino-app-cli /usr/local/bin/
COPY stubs/systemctl stubs/hostnamectl /usr/bin/
COPY stubs/fake-apt-db.json /var/lib/fake-apt/db.json
COPY stubs/apt-get /usr/local/lib/fake-apt-get
COPY sudoers-arduino /etc/sudoers.d/arduino
# apt-get goes last: everything above still needed the real one.
RUN chmod 755 /usr/local/bin/arduino-app-cli /usr/bin/systemctl /usr/bin/hostnamectl \
    && chmod 440 /etc/sudoers.d/arduino && chmod 644 /var/lib/fake-apt/db.json \
    && install -m 666 /dev/null /var/log/fake-systemctl.log \
    && mv /usr/bin/apt-get /usr/bin/apt-get.real && install -m 755 /usr/local/lib/fake-apt-get /usr/bin/apt-get

EXPOSE 22
CMD ["/usr/sbin/sshd", "-D", "-e"]
