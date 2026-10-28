# A job image for Android builds: the act-24.04 runner image (node and git,
# which act needs to run actions) plus OpenJDK 17, the Android SDK
# (platform-tools, platform 35, build-tools 35.0.0) and Gradle.
#
# Android's build tools (aapt2, d8, zipalign) only exist for x86_64 Linux, so
# the image is built and run as linux/amd64. On an Apple silicon Mac Docker
# Desktop emulates it; on an amd64 Linux host it runs natively.
#
#   docker build --platform linux/amd64 -t selfaction/android:35 images/android
#   selfaction worker --server https://ci.example.com -P android=selfaction/android:35 --container-architecture linux/amd64
#
# On an amd64 Linux host --container-architecture is not needed. Jobs then use
# `runs-on: android` and build with `gradle assembleDebug` (no wrapper needed).
FROM catthehacker/ubuntu:act-24.04

ARG CMDLINE_TOOLS=16111833
ARG CMDLINE_TOOLS_SHA1=e025545c62a8e64c7559119566a569fb1dec5f60
ARG GRADLE=8.14.6
ARG GRADLE_SHA256=7988ed071b2a07900e2ec715fca15c6ab72bce6db433af301fa7fa7e9407bd24
ARG PLATFORM=android-35
ARG BUILD_TOOLS=35.0.0

ENV ANDROID_HOME=/opt/android-sdk \
    ANDROID_SDK_ROOT=/opt/android-sdk \
    JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64 \
    GRADLE_HOME=/opt/gradle
ENV PATH=$JAVA_HOME/bin:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools:$GRADLE_HOME/bin:$PATH

RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openjdk-17-jdk-headless unzip \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL -o /tmp/tools.zip "https://dl.google.com/android/repository/commandlinetools-linux-${CMDLINE_TOOLS}_latest.zip" \
    && echo "${CMDLINE_TOOLS_SHA1}  /tmp/tools.zip" | sha1sum -c - \
    && mkdir -p "$ANDROID_HOME/cmdline-tools" \
    && unzip -q /tmp/tools.zip -d "$ANDROID_HOME/cmdline-tools" \
    && mv "$ANDROID_HOME/cmdline-tools/cmdline-tools" "$ANDROID_HOME/cmdline-tools/latest" \
    && rm /tmp/tools.zip \
    && (yes || true) | sdkmanager --licenses >/dev/null \
    && sdkmanager --install "platform-tools" "platforms;${PLATFORM}" "build-tools;${BUILD_TOOLS}" >/dev/null \
    && chmod -R a+rX "$ANDROID_HOME"

RUN curl -fsSL -o /tmp/gradle.zip "https://services.gradle.org/distributions/gradle-${GRADLE}-bin.zip" \
    && echo "${GRADLE_SHA256}  /tmp/gradle.zip" | sha256sum -c - \
    && unzip -q /tmp/gradle.zip -d /opt \
    && mv "/opt/gradle-${GRADLE}" "$GRADLE_HOME" \
    && rm /tmp/gradle.zip \
    && gradle --version
