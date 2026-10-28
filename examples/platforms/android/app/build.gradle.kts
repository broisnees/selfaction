plugins {
    id("com.android.application")
}

android {
    namespace = "dev.selfaction.hello"
    compileSdk = 35

    defaultConfig {
        applicationId = "dev.selfaction.hello"
        minSdk = 24
        targetSdk = 35
        versionCode = 1
        versionName = "1.0"
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}
