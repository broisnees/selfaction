// A minimal Android app that selfaction builds in its Android job image
// (images/android): `gradle assembleDebug` writes
// app/build/outputs/apk/debug/app-debug.apk.
pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "hello"
include(":app")
